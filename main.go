package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	frpauth "github.com/fatedier/frp/pkg/auth"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/metadata"
	"github.com/go-gost/core/service"
	gostconfig "github.com/go-gost/x/config"
	gostcmd "github.com/go-gost/x/config/cmd"
	"github.com/go-gost/x/config/loader"
	service_parser "github.com/go-gost/x/config/parsing/service"
	xctx "github.com/go-gost/x/ctx"
	"github.com/go-gost/x/registry"
	"github.com/spf13/pflag"

	"github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/policy/featuregate"
	"github.com/fatedier/frp/pkg/policy/security"
	"github.com/fatedier/frp/pkg/util/log"
	netpkg "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/system"
	libio "github.com/fatedier/golib/io"

	_ "github.com/go-gost/x/handler/auto"
	_ "github.com/go-gost/x/handler/http"
	_ "github.com/go-gost/x/handler/socks/v4"
	_ "github.com/go-gost/x/handler/socks/v5"
	_ "github.com/go-gost/x/handler/ss"
	_ "github.com/go-gost/x/listener/tcp"
)

var (
	clientEncryptionKey []byte

	frpListenerOnce sync.Once
	frpListenerMu   sync.Mutex
	frpListeners    = map[string]*frpListener{}
)

func main() {
	system.EnableCompatibilityMode()

	var (
		cfgFile          = pflag.StringP("config", "c", "./frpc.ini", "config file of frpc")
		strictConfigMode = pflag.Bool("strict_config", true, "strict config parsing mode, unknown fields will cause an errors")
		allowUnsafe      = pflag.StringSlice("allow-unsafe", []string{}, fmt.Sprintf("allowed unsafe features, one or more of: %s", strings.Join(security.ClientUnsafeFeatures, ", ")))
		serviceCmd       = pflag.StringP("service", "H", "ss://", "gost Service cmd (same format as gost -L)")
	)
	pflag.Parse()

	unsafeFeatures := security.NewUnsafeFeatures(*allowUnsafe)

	clientCfg, proxyCfgs, visitorCfgs, isLegacyFormat, err := config.LoadClientConfig(*cfgFile, *strictConfigMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if isLegacyFormat {
		fmt.Fprintln(os.Stderr, "WARNING: ini format is deprecated, please use yaml/json/toml format instead!")
	}

	if len(clientCfg.FeatureGates) > 0 {
		if err := featuregate.SetFromMap(clientCfg.FeatureGates); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	warning, err := validation.ValidateAllClientConfig(clientCfg, proxyCfgs, visitorCfgs, unsafeFeatures)
	if warning != nil {
		fmt.Fprintf(os.Stderr, "WARNING: %v\n", warning)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	authRuntime, err := frpauth.BuildClientAuth(&clientCfg.Auth)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	clientEncryptionKey = authRuntime.EncryptionKey()

	log.InitLogger(clientCfg.Log.To, clientCfg.Log.Level, int(clientCfg.Log.MaxDays), clientCfg.Log.DisablePrintColor)
	log.Infof("start tcpconn client for config file [%s]", *cfgFile)
	defer log.Infof("tcpconn client for config file [%s] stopped", *cfgFile)
	gostService, err := createGostService(*serviceCmd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	svr, err := client.NewService(client.ServiceOptions{
		Common:         clientCfg,
		ProxyCfgs:      proxyCfgs,
		VisitorCfgs:    visitorCfgs,
		UnsafeFeatures: unsafeFeatures,
		ConfigFilePath: *cfgFile,
		HandleWorkConnCb: func(baseCfg *v1.ProxyBaseConfig, workConn net.Conn, m *msg.StartWorkConn) bool {
			return handleTCPConnWorkConn(gostService, baseCfg, workConn, m)
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		cancel()
	}()

	if err := svr.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func handleTCPConnWorkConn(serviceInstance service.Service, baseCfg *v1.ProxyBaseConfig, workConn net.Conn, m *msg.StartWorkConn) (continueDefault bool) {
	handler, ok := serviceInstance.(interface {
		HandleConn(net.Conn) error
	})
	if !ok {
		log.Warnf("gost service does not support direct connection handling, fallback to default")
		return true
	}

	var remote io.ReadWriteCloser = workConn
	if baseCfg.Transport.UseEncryption {
		if len(clientEncryptionKey) == 0 {
			workConn.Close()
			log.Errorf("encryption enabled but client encryption key is empty")
			return false
		}
		var err error
		remote, err = libio.WithEncryption(remote, clientEncryptionKey)
		if err != nil {
			workConn.Close()
			log.Errorf("create encryption stream error: %v", err)
			return false
		}
	}

	if baseCfg.Transport.UseCompression {
		rwc, recycleFn := libio.WithCompressionFromPool(remote)
		if recycleFn != nil {
			remote = &recycleReadWriteCloser{
				ReadWriteCloser: rwc,
				recycle:         recycleFn,
			}
		} else {
			remote = rwc
		}
	}

	conn := netpkg.WrapReadWriteCloserToConn(remote, workConn)

	ctx := context.Background()
	if m != nil && m.SrcAddr != "" && m.SrcPort != 0 {
		if srcAddr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(m.SrcAddr, strconv.Itoa(int(m.SrcPort)))); err == nil {
			ctx = xctx.ContextWithSrcAddr(ctx, srcAddr)
			conn.SetRemoteAddr(srcAddr)
		}
	}
	if m != nil && m.DstAddr != "" && m.DstPort != 0 {
		if dstAddr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(m.DstAddr, strconv.Itoa(int(m.DstPort)))); err == nil {
			ctx = xctx.ContextWithDstAddr(ctx, dstAddr)
		}
	}

	ctxConn := netpkg.NewContextConn(ctx, conn)
	if err := handler.HandleConn(ctxConn); err != nil {
		log.Errorf("gost handle conn error: %v", err)
		_ = ctxConn.Close()
		return false
	}
	return false
}
func createGostService(cmd string) (service.Service, error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil, fmt.Errorf("gost service cmd is empty")
	}

	registerFrpListener()

	cfg, err := gostcmd.BuildConfigFromCmd([]string{cmd}, nil)
	if err != nil {
		return nil, err
	}
	if len(cfg.Services) == 0 {
		return nil, fmt.Errorf("no service config generated from cmd")
	}
	if len(cfg.Services) > 1 {
		return nil, fmt.Errorf("multiple service configs are not supported yet")
	}

	svcCfg := cfg.Services[0]
	if svcCfg.Listener == nil {
		svcCfg.Listener = &gostconfig.ListenerConfig{}
	}
	svcCfg.Listener.Type = "frp"

	cfg.Services = nil
	gostconfig.Set(cfg)
	if err := loader.Load(cfg); err != nil {
		return nil, err
	}

	svc, err := service_parser.ParseService(svcCfg)
	if err != nil {
		return nil, err
	}

	frpListenerMu.Lock()
	ln := frpListeners[svcCfg.Name]
	frpListenerMu.Unlock()
	if ln == nil {
		return nil, fmt.Errorf("frp listener is not initialized")
	}

	wrapped := &frpService{
		Service: svc,
		ln:      ln,
	}
	go func() {
		if err := svc.Serve(); err != nil {
			log.Errorf("gost service serve error: %v", err)
		}
	}()
	return wrapped, nil
}

type recycleReadWriteCloser struct {
	io.ReadWriteCloser
	recycle func()
}

func (r *recycleReadWriteCloser) Close() error {
	if r.recycle != nil {
		r.recycle()
	}
	return r.ReadWriteCloser.Close()
}

type frpService struct {
	service.Service
	ln *frpListener
}

func (s *frpService) HandleConn(conn net.Conn) error {
	return s.ln.PutConn(conn)
}

type frpListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
	addr      net.Addr
}

func registerFrpListener() {
	frpListenerOnce.Do(func() {
		registry.ListenerRegistry().Register("frp", newFrpListener)
	})
}

func newFrpListener(opts ...listener.Option) listener.Listener {
	options := listener.Options{}
	for _, opt := range opts {
		opt(&options)
	}

	addr := frpAddr("frp")
	if options.Addr != "" {
		addr = frpAddr(options.Addr)
	}

	ln := &frpListener{
		conns:  make(chan net.Conn, 128),
		closed: make(chan struct{}),
		addr:   addr,
	}

	frpListenerMu.Lock()
	frpListeners[options.Service] = ln
	frpListenerMu.Unlock()

	return ln
}

func (l *frpListener) Init(_ metadata.Metadata) error {
	return nil
}

func (l *frpListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, listener.ErrClosed
	case conn, ok := <-l.conns:
		if !ok {
			return nil, listener.ErrClosed
		}
		return conn, nil
	}
}

func (l *frpListener) Addr() net.Addr {
	return l.addr
}

func (l *frpListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		close(l.conns)
	})
	return nil
}

func (l *frpListener) PutConn(conn net.Conn) error {
	select {
	case <-l.closed:
		return listener.ErrClosed
	default:
	}

	select {
	case l.conns <- conn:
		return nil
	case <-l.closed:
		return listener.ErrClosed
	}
}

type frpAddr string

func (a frpAddr) Network() string {
	return "frp"
}

func (a frpAddr) String() string {
	return string(a)
}
