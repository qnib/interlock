package haproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/ehazlett/interlock"
	"github.com/ehazlett/interlock/plugins"
	"github.com/samalba/dockerclient"
)

const (
	haproxyTmpl = `# managed by interlock
global
    {{ if .PluginConfig.SyslogAddr }}log {{ .PluginConfig.SyslogAddr }} local0
    log-send-hostname{{ end }}
    maxconn {{ .PluginConfig.MaxConn }}
    pidfile {{ .PluginConfig.PidPath }}

defaults
    mode http
    retries 3
    option redispatch
    option httplog
    option dontlognull
    option http-server-close
    option forwardfor
    timeout connect {{ .PluginConfig.ConnectTimeout }}
    timeout client {{ .PluginConfig.ClientTimeout }}
    timeout server {{ .PluginConfig.ServerTimeout }}

frontend http-default
    bind *:{{ .PluginConfig.Port }}
    {{ if .PluginConfig.SSLCert }}bind *:{{ .PluginConfig.SSLPort }} ssl crt {{ .PluginConfig.SSLCert }} {{ .PluginConfig.SSLOpts }}{{ end }}
    monitor-uri /haproxy?monitor
    {{ if .PluginConfig.StatsUser }}stats realm Stats
    stats auth {{ .PluginConfig.StatsUser }}:{{ .PluginConfig.StatsPassword }}{{ end }}
    stats enable
    stats uri /haproxy?stats
    stats refresh 5s
    {{ range $host := .Hosts }}acl is_{{ $host.Name }} hdr_beg(host) {{ $host.Domain }}
    use_backend {{ $host.Name }} if is_{{ $host.Name }}
    {{ end }}
{{ range $host := .Hosts }}backend {{ $host.Name }}
    http-response add-header X-Request-Start %Ts.%ms
    balance roundrobin
    {{ range $option := $host.BackendOptions }}option {{ $option }}
    {{ end }}
    {{ if $host.Check }}option {{ $host.Check }}{{ end }}
    {{ if $host.SSLOnly }}redirect scheme https if !{ ssl_fc  }{{ end }}
    {{ range $i,$up := $host.Upstreams }}server {{ $host.Name }}_{{ $i }} {{ $up.Addr }} check inter {{ $up.CheckInterval }}
    {{ end }}
{{ end }}`
)

var (
	eventsErrChan = make(chan error)
	proxyCmd      *exec.Cmd
)

type HaproxyPlugin struct {
	interlockConfig *interlock.Config
	pluginConfig    *PluginConfig
	client          *dockerclient.DockerClient
	mux             sync.Mutex
}

func init() {
	plugins.Register(
		pluginInfo.Name,
		&plugins.RegisteredPlugin{
			New: NewPlugin,
			Info: func() *interlock.PluginInfo {
				return pluginInfo
			},
		})
}

func logMessage(level log.Level, args ...string) {
	plugins.Log(pluginInfo.Name, level, args...)
}

func loadPluginConfig() (*PluginConfig, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	cfg := &PluginConfig{
		ProxyConfigPath:             filepath.Join(wd, "proxy.conf"),
		ProxyBackendOverrideAddress: "",
		Port:           8080,
		PidPath:        filepath.Join(wd, "proxy.pid"),
		MaxConn:        2048,
		ConnectTimeout: 5000,
		ServerTimeout:  10000,
		ClientTimeout:  10000,
		StatsUser:      "stats",
		StatsPassword:  "interlock",
		SSLCert:        "",
		SSLPort:        8443,
		SSLOpts:        "",
	}

	// load custom config via environment
	proxyConfigPath := os.Getenv("HAPROXY_PROXY_CONFIG_PATH")
	if proxyConfigPath != "" {
		cfg.ProxyConfigPath = proxyConfigPath
	}

	proxyBackendOverrideAddress := os.Getenv("HAPROXY_PROXY_BACKEND_OVERRIDE_ADDRESS")
	if proxyBackendOverrideAddress != "" {
		cfg.ProxyBackendOverrideAddress = proxyBackendOverrideAddress
	}

	port := os.Getenv("HAPROXY_PORT")
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil {
			return nil, err
		}
		cfg.Port = p
	}

	proxyPidPath := os.Getenv("HAPROXY_PID_PATH")
	if proxyPidPath != "" {
		cfg.PidPath = proxyPidPath
	}

	maxConn := os.Getenv("HAPROXY_MAX_CONN")
	if maxConn != "" {
		c, err := strconv.Atoi(maxConn)
		if err != nil {
			return nil, err
		}
		cfg.MaxConn = c
	}

	connectTimeout := os.Getenv("HAPROXY_CONNECT_TIMEOUT")
	if connectTimeout != "" {
		c, err := strconv.Atoi(connectTimeout)
		if err != nil {
			return nil, err
		}
		cfg.ConnectTimeout = c
	}

	serverTimeout := os.Getenv("HAPROXY_SERVER_TIMEOUT")
	if serverTimeout != "" {
		c, err := strconv.Atoi(serverTimeout)
		if err != nil {
			return nil, err
		}
		cfg.ServerTimeout = c
	}

	clientTimeout := os.Getenv("HAPROXY_CLIENT_TIMEOUT")
	if clientTimeout != "" {
		c, err := strconv.Atoi(clientTimeout)
		if err != nil {
			return nil, err
		}
		cfg.ClientTimeout = c
	}

	statsUser := os.Getenv("HAPROXY_STATS_USER")
	if statsUser != "" {
		cfg.StatsUser = statsUser
	}

	statsPassword := os.Getenv("HAPROXY_STATS_PASSWORD")
	if statsPassword != "" {
		cfg.StatsPassword = statsPassword
	}

	sslPort := os.Getenv("HAPROXY_SSL_PORT")
	if sslPort != "" {
		p, err := strconv.Atoi(sslPort)
		if err != nil {
			return nil, err
		}
		cfg.SSLPort = p
	}

	sslCert := os.Getenv("HAPROXY_SSL_CERT")
	if sslCert != "" {
		cfg.SSLCert = sslCert
	}

	sslOpts := os.Getenv("HAPROXY_SSL_OPTS")
	if sslOpts != "" {
		cfg.SSLOpts = sslOpts
	}

	return cfg, nil
}

func NewPlugin(interlockConfig *interlock.Config, client *dockerclient.DockerClient) (interlock.Plugin, error) {
	pluginConfig, err := loadPluginConfig()
	if err != nil {
		return nil, err
	}

	plugin := HaproxyPlugin{
		pluginConfig:    pluginConfig,
		interlockConfig: interlockConfig,
		client:          client,
	}

	return plugin, nil
}

func (p HaproxyPlugin) Init() error {
	if err := p.updateConfig(nil); err != nil {
		return err
	}

	if err := p.reload(); err != nil {
		return err
	}

	return nil
}

func (p HaproxyPlugin) Info() *interlock.PluginInfo {
	return &interlock.PluginInfo{
		Name:        pluginName,
		Version:     pluginVersion,
		Description: pluginDescription,
		Url:         pluginUrl,
	}
}

func (p HaproxyPlugin) handleUpdate(event *dockerclient.Event) error {
	logMessage(log.DebugLevel, "update request received")
	if err := p.updateConfig(event); err != nil {
		return err
	}

	if err := p.reload(); err != nil {
		return err
	}

	return nil
}

func (p HaproxyPlugin) HandleEvent(event *dockerclient.Event) error {
	switch event.Status {
	case "stop", "kill", "die":
		// add delay to make sure container is removed
		time.Sleep(250 * time.Millisecond)
	case "interlock-stop":
		// stop haproxy
		if proxyCmd != nil {
			pid, err := p.getProxyPid()
			if err != nil {
				return err
			}
			logMessage(log.DebugLevel, fmt.Sprintf("stopping haproxy pid=%d", pid))
			syscall.Kill(pid, syscall.SIGTERM)
		}
		// wait for stop
		time.Sleep(1 * time.Second)
		return nil
	}

	if err := p.handleUpdate(event); err != nil {
		return err
	}

	return nil
}

func (p HaproxyPlugin) writeConfig(config *ProxyConfig) error {
	p.mux.Lock()
	defer p.mux.Unlock()
	f, err := os.OpenFile(p.pluginConfig.ProxyConfigPath, os.O_WRONLY|os.O_TRUNC, 0664)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		ff, fErr := os.Create(p.pluginConfig.ProxyConfigPath)
		defer ff.Close()
		if fErr != nil {
			return fErr
		}
		f = ff
	}
	defer f.Close()
	t := template.New("haproxy")
	tmpl, err := t.Parse(haproxyTmpl)
	if err != nil {
		return err
	}
	var c bytes.Buffer
	if err := tmpl.Execute(&c, config); err != nil {
		return err
	}
	_, fErr := f.Write(c.Bytes())
	if fErr != nil {
		return fErr
	}
	f.Sync()
	return nil
}

func (p HaproxyPlugin) GenerateProxyConfig(isKillEvent bool) (*ProxyConfig, error) {
	logMessage(log.DebugLevel, "generating proxy config")

	containers, err := p.client.ListContainers(false, false, "")
	if err != nil {
		return nil, err
	}
	var hosts []*Host
	proxyUpstreams := map[string][]*Upstream{}
	hostChecks := map[string]string{}
	hostBackendOptions := map[string][]string{}
	hostSSLOnly := map[string]bool{}
	for _, cnt := range containers {
		cntId := cnt.Id[:12]
		// load interlock data
		cInfo, err := p.client.InspectContainer(cntId)
		if err != nil {
			return nil, err
		}
		env := cInfo.Config.Env
		interlockData := &interlock.InterlockData{}
		for _, e := range env {

			envParts := strings.Split(e, "=")
			if envParts[0] == "INTERLOCK_DATA" {
				b := bytes.NewBufferString(envParts[1])
				if err := json.NewDecoder(b).Decode(&interlockData); err != nil {
					log.Warnf("%s: unable to parse interlock data: %s", cntId, err)
				}
				break
			}
		}
		hostname := cInfo.Config.Hostname
		domain := cInfo.Config.Domainname
		if interlockData.Hostname != "" {
			hostname = interlockData.Hostname
		}
		if interlockData.Domain != "" {
			domain = interlockData.Domain
		}
		if domain == "" {
			continue
		}
		if hostname != domain && hostname != "" {
			domain = fmt.Sprintf("%s.%s", hostname, domain)
		}
		if interlockData.Check != "" {
			if val, ok := hostChecks[domain]; ok {
				// check existing host check for different values
				if val != interlockData.Check {
					log.Warnf("conflicting check specified for %s", domain)
				}
			} else {
				hostChecks[domain] = interlockData.Check
				log.Infof("using custom check for %s: %s", domain, interlockData.Check)
			}
		}
		checkInterval := 5000
		if interlockData.CheckInterval != 0 {
			checkInterval = interlockData.CheckInterval
			log.Infof("using custom check interval for %s: %d", domain, checkInterval)
		}
		if len(interlockData.BackendOptions) > 0 {
			hostBackendOptions[domain] = interlockData.BackendOptions
			log.Infof("using backend options for %s: %s", domain, strings.Join(interlockData.BackendOptions, ","))
		}
		hostSSLOnly[domain] = false
		if interlockData.SSLOnly {
			log.Infof("configuring ssl redirect for %s", domain)
			hostSSLOnly[domain] = true
		}

		//host := cInfo.NetworkSettings.IpAddress
		ports := cInfo.NetworkSettings.Ports
		if len(ports) == 0 {
			log.Warnf("%s: no ports exposed", cntId)
			continue
		}
		var portDef dockerclient.PortBinding

		for _, v := range ports {
			if len(v) > 0 {
				portDef = dockerclient.PortBinding{
					HostIp:   v[0].HostIp,
					HostPort: v[0].HostPort,
				}
				break
			}
		}

		if p.pluginConfig.ProxyBackendOverrideAddress != "" {
			portDef.HostIp = p.pluginConfig.ProxyBackendOverrideAddress
		}

		addr := fmt.Sprintf("%s:%s", portDef.HostIp, portDef.HostPort)

		if interlockData.Port != 0 {
			for k, v := range ports {
				parts := strings.Split(k, "/")
				if parts[0] == string(interlockData.Port) {
					port := v[0]
					log.Infof("using port %s", port.HostPort)
					addr = fmt.Sprintf("%s:%s", port.HostIp, port.HostPort)
					break
				}
			}
		}

		up := &Upstream{
			Addr:          addr,
			CheckInterval: checkInterval,
		}
		for _, alias := range interlockData.AliasDomains {
			log.Infof("adding alias %s for %s", alias, cntId)
			proxyUpstreams[alias] = append(proxyUpstreams[alias], up)
		}
		proxyUpstreams[domain] = append(proxyUpstreams[domain], up)
		if !isKillEvent && interlockData.Warm {
			log.Infof("warming %s: %s", cntId, addr)
			http.Get(fmt.Sprintf("http://%s", addr))
		}

	}
	for k, v := range proxyUpstreams {
		name := strings.Replace(k, ".", "_", -1)
		host := &Host{
			Name:           name,
			Domain:         k,
			Upstreams:      v,
			Check:          hostChecks[k],
			BackendOptions: hostBackendOptions[k],
			SSLOnly:        hostSSLOnly[k],
		}
		log.Infof("adding host name=%s domain=%s", host.Name, host.Domain)
		hosts = append(hosts, host)
	}
	// generate config
	cfg := &ProxyConfig{
		Hosts:        hosts,
		PluginConfig: p.pluginConfig,
	}
	return cfg, nil
}

func (p HaproxyPlugin) updateConfig(e *dockerclient.Event) error {
	isKillEvent := false
	if e != nil && e.Status == "kill" {
		isKillEvent = true
	}
	cfg, err := p.GenerateProxyConfig(isKillEvent)
	if err != nil {
		return err
	}
	if err := p.writeConfig(cfg); err != nil {
		return err
	}
	return nil
}

func (p HaproxyPlugin) getProxyPid() (int, error) {
	f, err := ioutil.ReadFile(p.pluginConfig.PidPath)
	if err != nil {
		return -1, err
	}
	buf := bytes.NewBuffer(f)
	pd := buf.String()
	pd = strings.TrimSpace(pd)
	pid, err := strconv.Atoi(pd)
	if err != nil {
		return -1, err
	}
	return pid, nil
}

func (p HaproxyPlugin) reload() error {
	args := []string{"-D", "-f", p.pluginConfig.ProxyConfigPath, "-p", p.pluginConfig.PidPath, "-sf"}
	if proxyCmd != nil {
		p, err := p.getProxyPid()
		if err != nil {
			log.Error(err)
		}
		pid := strconv.Itoa(p)
		args = append(args, pid)
	}

	haproxyPath, err := exec.LookPath("haproxy")
	if err != nil {
		return err
	}

	cmd := exec.Command(haproxyPath, args...)
	if err := cmd.Run(); err != nil {
		return err
	}
	proxyCmd = cmd

	logMessage(log.InfoLevel, "proxy reloaded and ready")
	return nil
}
