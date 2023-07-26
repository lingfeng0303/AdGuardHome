package home

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
	"unicode/utf8"

	"github.com/AdguardTeam/AdGuardHome/internal/aghalg"
	"github.com/AdguardTeam/AdGuardHome/internal/aghhttp"
	"github.com/AdguardTeam/AdGuardHome/internal/aghnet"
	"github.com/AdguardTeam/AdGuardHome/internal/version"
	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/log"
	"github.com/quic-go/quic-go/http3"
)

// getAddrsResponse is the response for /install/get_addresses endpoint.
type getAddrsResponse struct {
	Interfaces map[string]*aghnet.NetInterface `json:"interfaces"`

	// Version is the version of AdGuard Home.
	//
	// TODO(a.garipov): In the new API, rename this endpoint to something more
	// general, since there will be more information here than just network
	// interfaces.
	Version string `json:"version"`

	WebPort int `json:"web_port"`
	DNSPort int `json:"dns_port"`
}

// handleInstallGetAddresses is the handler for /install/get_addresses endpoint.
func (web *webAPI) handleInstallGetAddresses(w http.ResponseWriter, r *http.Request) {
	data := getAddrsResponse{
		Version: version.Version(),

		WebPort: defaultPortHTTP,
		DNSPort: defaultPortDNS,
	}

	ifaces, err := aghnet.GetValidNetInterfacesForWeb()
	if err != nil {
		aghhttp.Error(r, w, http.StatusInternalServerError, "Couldn't get interfaces: %s", err)

		return
	}

	data.Interfaces = make(map[string]*aghnet.NetInterface)
	for _, iface := range ifaces {
		data.Interfaces[iface.Name] = iface
	}

	_ = aghhttp.WriteJSONResponse(w, r, data)
}

type checkConfReqEnt struct {
	IP      netip.Addr `json:"ip"`
	Port    int        `json:"port"`
	Autofix bool       `json:"autofix"`
}

type checkConfReq struct {
	Web         checkConfReqEnt `json:"web"`
	DNS         checkConfReqEnt `json:"dns"`
	SetStaticIP bool            `json:"set_static_ip"`
}

type checkConfRespEnt struct {
	Status     string `json:"status"`
	CanAutofix bool   `json:"can_autofix"`
}

type staticIPJSON struct {
	Static string `json:"static"`
	IP     string `json:"ip"`
	Error  string `json:"error"`
}

type checkConfResp struct {
	StaticIP staticIPJSON     `json:"static_ip"`
	Web      checkConfRespEnt `json:"web"`
	DNS      checkConfRespEnt `json:"dns"`
}

// validateWeb returns error is the web part if the initial configuration can't
// be set.
func (req *checkConfReq) validateWeb(tcpPorts aghalg.UniqChecker[tcpPort]) (err error) {
	defer func() { err = errors.Annotate(err, "validating ports: %w") }()

	// TODO(a.garipov): Declare all port variables anywhere as uint16.
	reqPort := uint16(req.Web.Port)
	port := tcpPort(reqPort)
	addPorts(tcpPorts, port)
	if err = tcpPorts.Validate(); err != nil {
		// Reset the value for the port to 1 to make sure that validateDNS
		// doesn't throw the same error, unless the same TCP port is set there
		// as well.
		tcpPorts[port] = 1

		return err
	}

	switch reqPort {
	case 0, config.HTTPConfig.Address.Port():
		return nil
	default:
		// Go on and check the port binding only if it's not zero or won't be
		// unbound after install.
	}

	return aghnet.CheckPort("tcp", netip.AddrPortFrom(req.Web.IP, reqPort))
}

// validateDNS returns error if the DNS part of the initial configuration can't
// be set.  canAutofix is true if the port can be unbound by AdGuard Home
// automatically.
func (req *checkConfReq) validateDNS(
	tcpPorts aghalg.UniqChecker[tcpPort],
) (canAutofix bool, err error) {
	defer func() { err = errors.Annotate(err, "validating ports: %w") }()

	port := uint16(req.DNS.Port)
	switch port {
	case 0:
		return false, nil
	case config.HTTPConfig.Address.Port():
		// Go on and only check the UDP port since the TCP one is already bound
		// by AdGuard Home for web interface.
	default:
		// Check TCP as well.
		addPorts(tcpPorts, tcpPort(port))
		if err = tcpPorts.Validate(); err != nil {
			return false, err
		}

		err = aghnet.CheckPort("tcp", netip.AddrPortFrom(req.DNS.IP, uint16(port)))
		if err != nil {
			return false, err
		}
	}

	err = aghnet.CheckPort("udp", netip.AddrPortFrom(req.DNS.IP, uint16(port)))
	if !aghnet.IsAddrInUse(err) {
		return false, err
	}

	// Try to fix automatically.
	canAutofix = checkDNSStubListener()
	if canAutofix && req.DNS.Autofix {
		if derr := disableDNSStubListener(); derr != nil {
			log.Error("disabling DNSStubListener: %s", err)
		}

		err = aghnet.CheckPort("udp", netip.AddrPortFrom(req.DNS.IP, uint16(port)))
		canAutofix = false
	}

	return canAutofix, err
}

// handleInstallCheckConfig handles the /check_config endpoint.
func (web *webAPI) handleInstallCheckConfig(w http.ResponseWriter, r *http.Request) {
	req := &checkConfReq{}

	err := json.NewDecoder(r.Body).Decode(req)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "decoding the request: %s", err)

		return
	}

	resp := &checkConfResp{}
	tcpPorts := aghalg.UniqChecker[tcpPort]{}
	if err = req.validateWeb(tcpPorts); err != nil {
		resp.Web.Status = err.Error()
	}

	if resp.DNS.CanAutofix, err = req.validateDNS(tcpPorts); err != nil {
		resp.DNS.Status = err.Error()
	} else if !req.DNS.IP.IsUnspecified() {
		resp.StaticIP = handleStaticIP(req.DNS.IP, req.SetStaticIP)
	}

	_ = aghhttp.WriteJSONResponse(w, r, resp)
}

// handleStaticIP - handles static IP request
// It either checks if we have a static IP
// Or if set=true, it tries to set it
func handleStaticIP(ip netip.Addr, set bool) staticIPJSON {
	resp := staticIPJSON{}

	interfaceName := aghnet.InterfaceByIP(ip)
	resp.Static = "no"

	if len(interfaceName) == 0 {
		resp.Static = "error"
		resp.Error = fmt.Sprintf("Couldn't find network interface by IP %s", ip)
		return resp
	}

	if set {
		// Try to set static IP for the specified interface
		err := aghnet.IfaceSetStaticIP(interfaceName)
		if err != nil {
			resp.Static = "error"
			resp.Error = err.Error()
			return resp
		}
	}

	// Fallthrough here even if we set static IP
	// Check if we have a static IP and return the details
	isStaticIP, err := aghnet.IfaceHasStaticIP(interfaceName)
	if err != nil {
		resp.Static = "error"
		resp.Error = err.Error()
	} else {
		if isStaticIP {
			resp.Static = "yes"
		}
		resp.IP = aghnet.GetSubnet(interfaceName).String()
	}
	return resp
}

// Check if DNSStubListener is active
func checkDNSStubListener() bool {
	if runtime.GOOS != "linux" {
		return false
	}

	cmd := exec.Command("systemctl", "is-enabled", "systemd-resolved")
	log.Tracef("executing %s %v", cmd.Path, cmd.Args)
	_, err := cmd.Output()
	if err != nil || cmd.ProcessState.ExitCode() != 0 {
		log.Info("command %s has failed: %v code:%d",
			cmd.Path, err, cmd.ProcessState.ExitCode())
		return false
	}

	cmd = exec.Command("grep", "-E", "#?DNSStubListener=yes", "/etc/systemd/resolved.conf")
	log.Tracef("executing %s %v", cmd.Path, cmd.Args)
	_, err = cmd.Output()
	if err != nil || cmd.ProcessState.ExitCode() != 0 {
		log.Info("command %s has failed: %v code:%d",
			cmd.Path, err, cmd.ProcessState.ExitCode())
		return false
	}

	return true
}

const (
	resolvedConfPath = "/etc/systemd/resolved.conf.d/adguardhome.conf"
	resolvedConfData = `[Resolve]
DNS=127.0.0.1
DNSStubListener=no
`
)
const resolvConfPath = "/etc/resolv.conf"

// Deactivate DNSStubListener
func disableDNSStubListener() error {
	dir := filepath.Dir(resolvedConfPath)
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		return fmt.Errorf("os.MkdirAll: %s: %w", dir, err)
	}

	err = os.WriteFile(resolvedConfPath, []byte(resolvedConfData), 0o644)
	if err != nil {
		return fmt.Errorf("os.WriteFile: %s: %w", resolvedConfPath, err)
	}

	_ = os.Rename(resolvConfPath, resolvConfPath+".backup")
	err = os.Symlink("/run/systemd/resolve/resolv.conf", resolvConfPath)
	if err != nil {
		_ = os.Remove(resolvedConfPath) // remove the file we've just created
		return fmt.Errorf("os.Symlink: %s: %w", resolvConfPath, err)
	}

	cmd := exec.Command("systemctl", "reload-or-restart", "systemd-resolved")
	log.Tracef("executing %s %v", cmd.Path, cmd.Args)
	_, err = cmd.Output()
	if err != nil {
		return err
	}
	if cmd.ProcessState.ExitCode() != 0 {
		return fmt.Errorf("process %s exited with an error: %d",
			cmd.Path, cmd.ProcessState.ExitCode())
	}

	return nil
}

type applyConfigReqEnt struct {
	IP   netip.Addr `json:"ip"`
	Port int        `json:"port"`
}

type applyConfigReq struct {
	Username string `json:"username"`
	Password string `json:"password"`

	Web applyConfigReqEnt `json:"web"`
	DNS applyConfigReqEnt `json:"dns"`
}

// copyInstallSettings copies the installation parameters between two
// configuration structures.
func copyInstallSettings(dst, src *configuration) {
	dst.HTTPConfig = src.HTTPConfig
	dst.DNS.BindHosts = src.DNS.BindHosts
	dst.DNS.Port = src.DNS.Port
}

// shutdownTimeout is the timeout for shutting HTTP server down operation.
const shutdownTimeout = 5 * time.Second

// shutdownSrv shuts srv down and prints error messages to the log.
func shutdownSrv(ctx context.Context, srv *http.Server) {
	defer log.OnPanic("")

	if srv == nil {
		return
	}

	err := srv.Shutdown(ctx)
	if err == nil {
		return
	}

	const msgFmt = "shutting down http server %q: %s"
	if errors.Is(err, context.Canceled) {
		log.Debug(msgFmt, srv.Addr, err)
	} else {
		log.Error(msgFmt, srv.Addr, err)
	}
}

// shutdownSrv3 shuts srv down and prints error messages to the log.
//
// TODO(a.garipov): Think of a good way to merge with [shutdownSrv].
func shutdownSrv3(srv *http3.Server) {
	defer log.OnPanic("")

	if srv == nil {
		return
	}

	err := srv.Close()
	if err == nil {
		return
	}

	const msgFmt = "shutting down http/3 server %q: %s"
	if errors.Is(err, context.Canceled) {
		log.Debug(msgFmt, srv.Addr, err)
	} else {
		log.Error(msgFmt, srv.Addr, err)
	}
}

// PasswordMinRunes is the minimum length of user's password in runes.
const PasswordMinRunes = 8

// Apply new configuration, start DNS server, restart Web server
func (web *webAPI) handleInstallConfigure(w http.ResponseWriter, r *http.Request) {
	req, restartHTTP, err := decodeApplyConfigReq(r.Body)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "%s", err)

		return
	}

	if utf8.RuneCountInString(req.Password) < PasswordMinRunes {
		aghhttp.Error(
			r,
			w,
			http.StatusUnprocessableEntity,
			"password must be at least %d symbols long",
			PasswordMinRunes,
		)

		return
	}

	err = aghnet.CheckPort("udp", netip.AddrPortFrom(req.DNS.IP, uint16(req.DNS.Port)))
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "%s", err)

		return
	}

	err = aghnet.CheckPort("tcp", netip.AddrPortFrom(req.DNS.IP, uint16(req.DNS.Port)))
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "%s", err)

		return
	}

	curConfig := &configuration{}
	copyInstallSettings(curConfig, config)

	Context.firstRun = false
	config.HTTPConfig.Address = netip.AddrPortFrom(req.Web.IP, uint16(req.Web.Port))
	config.DNS.BindHosts = []netip.Addr{req.DNS.IP}
	config.DNS.Port = req.DNS.Port

	// TODO(e.burkov): StartMods() should be put in a separate goroutine at the
	// moment we'll allow setting up TLS in the initial configuration or the
	// configuration itself will use HTTPS protocol, because the underlying
	// functions potentially restart the HTTPS server.
	err = startMods()
	if err != nil {
		Context.firstRun = true
		copyInstallSettings(config, curConfig)
		aghhttp.Error(r, w, http.StatusInternalServerError, "%s", err)

		return
	}

	u := &webUser{
		Name: req.Username,
	}
	Context.auth.UserAdd(u, req.Password)

	err = config.write()
	if err != nil {
		Context.firstRun = true
		copyInstallSettings(config, curConfig)
		aghhttp.Error(r, w, http.StatusInternalServerError, "Couldn't write config: %s", err)

		return
	}

	web.conf.firstRun = false
	web.conf.BindHost = req.Web.IP
	web.conf.BindPort = req.Web.Port

	registerControlHandlers(web)

	aghhttp.OK(w)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	if !restartHTTP {
		return
	}

	// Method http.(*Server).Shutdown needs to be called in a separate goroutine
	// and with its own context, because it waits until all requests are handled
	// and will be blocked by it's own caller.
	go func(timeout time.Duration) {
		defer log.OnPanic("web")

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		shutdownSrv(ctx, web.httpServer)
	}(shutdownTimeout)
}

// decodeApplyConfigReq decodes the configuration, validates some parameters,
// and returns it along with the boolean indicating whether or not the HTTP
// server must be restarted.
func decodeApplyConfigReq(r io.Reader) (req *applyConfigReq, restartHTTP bool, err error) {
	req = &applyConfigReq{}
	err = json.NewDecoder(r).Decode(&req)
	if err != nil {
		return nil, false, fmt.Errorf("parsing request: %w", err)
	}

	if req.Web.Port == 0 || req.DNS.Port == 0 {
		return nil, false, errors.Error("ports cannot be 0")
	}

	addrPort := config.HTTPConfig.Address
	restartHTTP = addrPort.Addr() != req.Web.IP || int(addrPort.Port()) != req.Web.Port
	if restartHTTP {
		err = aghnet.CheckPort("tcp", netip.AddrPortFrom(req.Web.IP, uint16(req.Web.Port)))
		if err != nil {
			return nil, false, fmt.Errorf(
				"checking address %s:%d: %w",
				req.Web.IP.String(),
				req.Web.Port,
				err,
			)
		}
	}

	return req, restartHTTP, err
}

func (web *webAPI) registerInstallHandlers() {
	Context.mux.HandleFunc("/control/install/get_addresses", preInstall(ensureGET(web.handleInstallGetAddresses)))
	Context.mux.HandleFunc("/control/install/check_config", preInstall(ensurePOST(web.handleInstallCheckConfig)))
	Context.mux.HandleFunc("/control/install/configure", preInstall(ensurePOST(web.handleInstallConfigure)))
}
