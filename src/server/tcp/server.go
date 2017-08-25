/**
 * server.go - proxy server implementation
 *
 * @author Yaroslav Pogrebnyak <yyyaroslav@gmail.com>
 */

package tcp

import (
	"crypto/tls"
	"crypto/x509"
	"golang.org/x/crypto/acme/autocert"
	"io/ioutil"
	"net"
	"time"

	"../../balance"
	"../../config"
	"../../core"
	"../../discovery"
	"../../healthcheck"
	"../../logging"
	"../../stats"
	"../../utils"
	tlsutil "../../utils/tls"
	"../../utils/tls/sni"
	"../modules/access"
	"../scheduler"
)

/**
 * Server listens for client connections and
 * proxies it to backends
 */
type Server struct {

	/* Server friendly name */
	name string

	/* Listener */
	listener net.Listener

	/* Configuration */
	cfg config.Server

	/* Scheduler deals with discovery, balancing and healthchecks */
	scheduler scheduler.Scheduler

	/* Current clients connection */
	clients map[string]net.Conn

	/* Stats handler */
	statsHandler *stats.Handler

	/* ----- channels ----- */

	/* Channel for new connections */
	connect chan (*core.TcpContext)

	/* Channel for dropping connections or connectons to drop */
	disconnect chan (net.Conn)

	/* Stop channel */
	stop chan bool

	/* Tls config used to connect to backends */
	backendsTlsConfg *tls.Config

	/* ----- modules ----- */

	/* Access module checks if client is allowed to connect */
	access *access.Access
}

/**
 * Creates new server instance
 */
func New(name string, cfg config.Server) (*Server, error) {

	log := logging.For("server")

	var err error = nil
	statsHandler := stats.NewHandler(name)

	// Create server
	server := &Server{
		name:         name,
		cfg:          cfg,
		stop:         make(chan bool),
		disconnect:   make(chan net.Conn),
		connect:      make(chan *core.TcpContext),
		clients:      make(map[string]net.Conn),
		statsHandler: statsHandler,
		scheduler: scheduler.Scheduler{
			Balancer:     balance.New(cfg.Sni, cfg.Balance),
			Discovery:    discovery.New(cfg.Discovery.Kind, *cfg.Discovery),
			Healthcheck:  healthcheck.New(cfg.Healthcheck.Kind, *cfg.Healthcheck),
			StatsHandler: statsHandler,
		},
	}

	/* Add access if needed */
	if cfg.Access != nil {
		server.access, err = access.NewAccess(cfg.Access)
		if err != nil {
			return nil, err
		}
	}

	/* Add backend tls config if needed */
	if cfg.BackendsTls != nil {
		server.backendsTlsConfg, err = prepareBackendsTlsConfig(cfg)
		if err != nil {
			return nil, err
		}
	}

	log.Info("Creating '", name, "': ", cfg.Bind, " ", cfg.Balance, " ", cfg.Discovery.Kind, " ", cfg.Healthcheck.Kind)

	return server, nil
}

/**
 * Returns current server configuration
 */
func (this *Server) Cfg() config.Server {
	return this.cfg
}

/**
 * Start server
 */
func (this *Server) Start() error {

	go func() {

		for {
			select {
			case client := <-this.disconnect:
				this.HandleClientDisconnect(client)

			case ctx := <-this.connect:
				this.HandleClientConnect(ctx)

			case <-this.stop:
				this.scheduler.Stop()
				this.statsHandler.Stop()
				if this.listener != nil {
					this.listener.Close()
					for _, conn := range this.clients {
						conn.Close()
					}
				}
				this.clients = make(map[string]net.Conn)
				return
			}
		}
	}()

	// Start stats handler
	this.statsHandler.Start()

	// Start scheduler
	this.scheduler.Start()

	// Start listening
	if err := this.Listen(); err != nil {
		this.Stop()
		return err
	}

	return nil
}

/**
 * Handle client disconnection
 */
func (this *Server) HandleClientDisconnect(client net.Conn) {
	client.Close()
	delete(this.clients, client.RemoteAddr().String())
	this.statsHandler.Connections <- uint(len(this.clients))
}

/**
 * Handle new client connection
 */
func (this *Server) HandleClientConnect(ctx *core.TcpContext) {
	client := ctx.Conn
	log := logging.For("server")

	if *this.cfg.MaxConnections != 0 && len(this.clients) >= *this.cfg.MaxConnections {
		log.Warn("Too many connections to ", this.cfg.Bind)
		client.Close()
		return
	}

	this.clients[client.RemoteAddr().String()] = client
	this.statsHandler.Connections <- uint(len(this.clients))
	go func() {
		this.handle(ctx)
		this.disconnect <- client
	}()
}

/**
 * Stop, dropping all connections
 */
func (this *Server) Stop() {

	log := logging.For("server.Listen")
	log.Info("Stopping ", this.name)

	this.stop <- true
}

func (this *Server) wrap(conn net.Conn, sniEnabled bool, tlsConfig *tls.Config) {
	log := logging.For("server.Listen.wrap")

	var hostname string
	var err error

	if sniEnabled {
		var sniConn net.Conn
		sniConn, hostname, err = sni.Sniff(conn, utils.ParseDurationOrDefault(this.cfg.Sni.ReadTimeout, time.Second*2))

		if err != nil {
			log.Error("Failed to get / parse ClientHello for sni: ", err)
			conn.Close()
			return
		}

		conn = sniConn
	}

	if tlsConfig != nil {
		conn = tls.Server(conn, tlsConfig)
	}

	this.connect <- &core.TcpContext{
		hostname,
		conn,
	}

}

func (this *Server) makeTlsConfig() (*tls.Config, error) {
	log := logging.For("server.mapeTlsConfig")
	var crt tls.Certificate
	var err error

	if crt, err = tls.LoadX509KeyPair(this.cfg.Tls.CertPath, this.cfg.Tls.KeyPath); err != nil {
		log.Error(err)
		return nil, err
	}

	tlsConfig := &tls.Config{
		Certificates:             []tls.Certificate{crt},
		CipherSuites:             tlsutil.MapCiphers(this.cfg.Tls.Ciphers),
		PreferServerCipherSuites: this.cfg.Tls.PreferServerCiphers,
		MinVersion:               tlsutil.MapVersion(this.cfg.Tls.MinVersion),
		MaxVersion:               tlsutil.MapVersion(this.cfg.Tls.MaxVersion),
		SessionTicketsDisabled:   !this.cfg.Tls.SessionTickets,
	}

	return tlsConfig, nil
}

func (this *Server) makeAcmeTlsConfig() *tls.Config {
	certManager := autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(this.cfg.Acme.Hosts...),
		Cache:      autocert.DirCache("/tmp"),
	}

	tlsConfig := &tls.Config{
		GetCertificate: certManager.GetCertificate,
	}

	if this.cfg.Tls != nil {
		tlsConfig.CipherSuites = tlsutil.MapCiphers(this.cfg.Tls.Ciphers)
		tlsConfig.PreferServerCipherSuites = this.cfg.Tls.PreferServerCiphers
		tlsConfig.MinVersion = tlsutil.MapVersion(this.cfg.Tls.MinVersion)
		tlsConfig.MaxVersion = tlsutil.MapVersion(this.cfg.Tls.MaxVersion)
		tlsConfig.SessionTicketsDisabled = !this.cfg.Tls.SessionTickets
	}

	return tlsConfig
}

/**
 * Listen on specified port for a connections
 */
func (this *Server) Listen() (err error) {

	log := logging.For("server.Listen")

	// create tcp listener
	this.listener, err = net.Listen("tcp", this.cfg.Bind)

	var tlsConfig *tls.Config
	sniEnabled := this.cfg.Sni != nil

	if this.cfg.Protocol == "tls" {

		if this.cfg.Acme != nil {
			tlsConfig = this.makeAcmeTlsConfig()
		} else {
			tlsConfig, err = this.makeTlsConfig()
			if err != nil {
				log.Error(err)
				return err
			}
		}

	}

	if err != nil {
		log.Error("Error starting ", this.cfg.Protocol+" server: ", err)
		return err
	}

	go func() {
		for {
			conn, err := this.listener.Accept()

			if err != nil {
				log.Error(err)
				return
			}

			go this.wrap(conn, sniEnabled, tlsConfig)
		}
	}()

	return nil
}

/**
 * Handle incoming connection and prox it to backend
 */
func (this *Server) handle(ctx *core.TcpContext) {
	clientConn := ctx.Conn
	log := logging.For("server.handle")

	/* Check access if needed */
	if this.access != nil {
		if !this.access.Allows(&clientConn.RemoteAddr().(*net.TCPAddr).IP) {
			log.Debug("Client disallowed to connect ", clientConn.RemoteAddr())
			clientConn.Close()
			return
		}
	}

	log.Debug("Accepted ", clientConn.RemoteAddr(), " -> ", this.listener.Addr())

	/* Find out backend for proxying */
	var err error
	backend, err := this.scheduler.TakeBackend(ctx)
	if err != nil {
		log.Error(err, " Closing connection ", clientConn.RemoteAddr())
		return
	}

	/* Connect to backend */
	var backendConn net.Conn

	if this.cfg.BackendsTls != nil {
		backendConn, err = tls.DialWithDialer(&net.Dialer{
			Timeout: utils.ParseDurationOrDefault(*this.cfg.BackendConnectionTimeout, 0),
		}, "tcp", backend.Address(), this.backendsTlsConfg)

	} else {
		backendConn, err = net.DialTimeout("tcp", backend.Address(), utils.ParseDurationOrDefault(*this.cfg.BackendConnectionTimeout, 0))
	}

	if err != nil {
		this.scheduler.IncrementRefused(*backend)
		log.Error(err)
		return
	}
	this.scheduler.IncrementConnection(*backend)
	defer this.scheduler.DecrementConnection(*backend)

	/* Stat proxying */
	log.Debug("Begin ", clientConn.RemoteAddr(), " -> ", this.listener.Addr(), " -> ", backendConn.RemoteAddr())
	cs := proxy(clientConn, backendConn, utils.ParseDurationOrDefault(*this.cfg.BackendIdleTimeout, 0))
	bs := proxy(backendConn, clientConn, utils.ParseDurationOrDefault(*this.cfg.ClientIdleTimeout, 0))

	isTx, isRx := true, true
	for isTx || isRx {
		select {
		case s, ok := <-cs:
			isRx = ok
			this.scheduler.IncrementRx(*backend, s.CountWrite)
		case s, ok := <-bs:
			isTx = ok
			this.scheduler.IncrementTx(*backend, s.CountWrite)
		}
	}

	log.Debug("End ", clientConn.RemoteAddr(), " -> ", this.listener.Addr(), " -> ", backendConn.RemoteAddr())
}

func prepareBackendsTlsConfig(cfg config.Server) (*tls.Config, error) {

	log := logging.For("server.prepareBackendsTlsConfig")
	var err error

	result := &tls.Config{
		InsecureSkipVerify:       cfg.BackendsTls.IgnoreVerify,
		CipherSuites:             tlsutil.MapCiphers(cfg.BackendsTls.Ciphers),
		PreferServerCipherSuites: cfg.BackendsTls.PreferServerCiphers,
		MinVersion:               tlsutil.MapVersion(cfg.BackendsTls.MinVersion),
		MaxVersion:               tlsutil.MapVersion(cfg.BackendsTls.MaxVersion),
		SessionTicketsDisabled:   !cfg.BackendsTls.SessionTickets,
	}

	if cfg.BackendsTls.CertPath != nil && cfg.BackendsTls.KeyPath != nil {

		var crt tls.Certificate

		if crt, err = tls.LoadX509KeyPair(*cfg.BackendsTls.CertPath, *cfg.BackendsTls.KeyPath); err != nil {
			log.Error(err)
			return nil, err
		}

		result.Certificates = []tls.Certificate{crt}
	}

	if cfg.BackendsTls.RootCaCertPath != nil {

		var caCertPem []byte

		if caCertPem, err = ioutil.ReadFile(*cfg.BackendsTls.RootCaCertPath); err != nil {
			log.Error(err)
			return nil, err
		}

		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(caCertPem); !ok {
			log.Error("Unable to load root pem")
		}

		result.RootCAs = caCertPool

	}

	return result, nil

}
