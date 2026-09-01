package server

import (
	"crypto/tls"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
)

var confFile string

var (
	genConfFile string
	genConfDir  bool
)

var confRead bool

var addresses AddressList

var (
	wsAddresses AddressList
	wsPath      string
	wsRaw       bool = DEFAULT_WS_RAW
)

var cert string

var key string

var gencertfile string

var logfile string

var loglevel int

var motd string

var motdAlwaysDisplay bool

var sendOrigin bool

var (
	screenShare    bool
	turnUrls       StringList
	turnSecret     string
	turnSecretFile string
	turnTtl        int
)

var sniRoutes SNIRouteList

var createDir bool

var Launch bool

var Servers []*Server

var (
	PID     int
	PID_STR string
	pidfile string
)

func Configure() error {
	PID = os.Getpid()
	PID_STR = strconv.Itoa(PID)

	flag.CommandLine.SetOutput(os.Stdout)

	flag.BoolVar(&createDir, "create", DEFAULT_CREATE_DIR, "Create directories upon any operation involving files being written to, or the working directory being changed.")

	flag.StringVar(&confFile, "conf-file", DEFAULT_CONF_FILE, "Path to a configuration file. If the configuration file does not exist, or there is an error reading the configuration file, the program will fall back to command line parameters.")

	flag.StringVar(&genConfFile, "gen-conf-file", DEFAULT_GEN_CONF_FILE, "Path to a configuration file to generate from command line parameters. If the configuration file can't be generated, an error message will be logged.")
	flag.BoolVar(&genConfDir, "gen-conf-dir", DEFAULT_GEN_CONF_DIR, "Whether or not to generate a configuration directory for the user. If the configuration directory and file can't be generated, an error message will be logged.")

	flag.BoolVar(&confRead, "conf-read", DEFAULT_CONF_READ, "Whether or not to read a configuration file. If a configuration file will not be read or searched for, the program will warn you. If you set a configuration file parameter, it will be reset to its default value.")

	flag.StringVar(&cert, "cert-file", DEFAULT_CERT_FILE, "SSL certificate file to use for the server's TLS connection, must point to an existing file. If this is empty, the server will automatically generate its own self-signed certificate.")
	flag.StringVar(&key, "key-file", DEFAULT_KEY_FILE, "SSL key to use for the server's TLS connection, must point to an existing file. If this is empty, the server will automatically generate its own self-signed certificate.")
	flag.StringVar(&gencertfile, "gen-cert-file", DEFAULT_GEN_CERT_FILE, "Generate a certificate file from the self-generated, self-signed SSL certificate. This file will only be created if you aren't loading your own certificate key files. The file will encode the key and certificate, packaging them both in a single .pem file.")

	flag.StringVar(&pidfile, "pid-file", DEFAULT_PID_FILE, "Create a PID file when the server has successfully started.")

	flag.Var(&addresses, "address", "Address the server will listen on in the format ip:port, such as \"0.0.0.0:6837\", \":6837\", \"[::]:6837\". The port must be between 1 and 65536. You can declare this parameter more than once for multiple listen addresses.")

	flag.Var(&wsAddresses, "ws-address", "Address the server will listen on for secure WebSocket (wss) connections, in the format ip:port, such as \":443\". This lets clients connect through corporate HTTP proxies. The port must be between 1 and 65536. You can declare this parameter more than once. If unset, no WebSocket listener is started.")
	flag.StringVar(&wsPath, "ws-path", DEFAULT_WS_PATH, "URL path on which the server accepts WebSocket connections, such as \"/\" or \"/ws\".")
	flag.BoolVar(&wsRaw, "ws-raw", DEFAULT_WS_RAW, "Also accept the historic newline delimited NVDA Remote protocol on the WebSocket addresses, by detecting which protocol a client speaks once the TLS handshake is complete. This lets older clients keep connecting to port 443. Set it to false to restore the WebSocket only behavior.")

	flag.IntVar(&loglevel, "log-level", DEFAULT_LOG_LEVEL, "Choose what log level you wish to use. Any value below -1 will be ignored.")
	flag.StringVar(&logfile, "log-file", DEFAULT_LOG_FILE, "Choose what log file you wish to use in addition to logging output to the console. If the file can't be created or open for writing, the program will fall back to console logging only.")

	flag.StringVar(&motd, "motd", DEFAULT_MOTD, "Display a message of the day for the server.")
	flag.BoolVar(&motdAlwaysDisplay, "motd-always-display", DEFAULT_MOTD_ALWAYS_DISPLAY, "Force the message of the day to be displayed upon each connection to the server, even if it hasn't changed.")

	flag.BoolVar(&sendOrigin, "send-origin", DEFAULT_SEND_ORIGIN, "Send an origin message from every message received by a client.")

	flag.BoolVar(&screenShare, "screen-share", DEFAULT_SCREEN_SHARE, "Enable relaying of WebRTC screen sharing signaling between clients that advertise the matching capability. Media never transits through the server. Disabled by default.")
	flag.Var(&turnUrls, "turn-url", "A STUN or TURN URL handed to clients requesting ICE servers, such as \"stun:turn.example.com:3478\" or \"turn:turn.example.com:3478?transport=udp\". You can declare this parameter more than once. Requires -screen-share.")
	flag.StringVar(&turnSecret, "turn-secret", DEFAULT_TURN_SECRET, "Shared secret used to derive ephemeral TURN credentials, matching the static-auth-secret of your coturn server. The secret is never sent to clients. Prefer -turn-secret-file, since command line parameters are visible to other users of the machine.")
	flag.StringVar(&turnSecretFile, "turn-secret-file", DEFAULT_TURN_SECRET_FILE, "Path to a file holding the shared secret used to derive ephemeral TURN credentials. This is preferred over -turn-secret, which exposes the secret in the process list. Leading and trailing whitespace is stripped.")
	flag.IntVar(&turnTtl, "turn-ttl", DEFAULT_TURN_TTL, "Lifetime in seconds of the ephemeral TURN credentials handed to clients.")

	flag.Var(&sniRoutes, "sni-route", "Forward the TLS connections asking for a given server name to another TCP endpoint, in the format name=host:port, such as \"turn.example.com=127.0.0.1:5349\". The connection is forwarded untouched before the handshake, so the target service presents its own certificate. This lets a TURN server share port 443 with this server, which is often the only port corporate networks allow. Connections asking for any other name, or for no name at all, are served normally. You can declare this parameter more than once.")

	flag.BoolVar(&Launch, "launch", DEFAULT_LAUNCH, "Launch the server.")

	flag.BoolVar(&adminEnabled, "admin", DEFAULT_ADMIN, "Enable the web administration dashboard. The login is fixed to \"administrator\" and the password is defined on the first connection to the dashboard. The dashboard requires a WebSocket listener (-ws-address) to be reachable.")
	flag.StringVar(&adminPasswordFile, "admin-password-file", DEFAULT_ADMIN_PASSWORD_FILE, "Path to the file used to store the bcrypt-hashed admin password so it survives restarts. If empty, the password is kept in memory only and must be set again after each restart. Use a path on a persistent volume when running in a container.")
	flag.StringVar(&adminPath, "admin-path", DEFAULT_ADMIN_PATH, "URL path on which the web administration dashboard is served, such as \"/admin\".")
	flag.StringVar(&adminDataFile, "admin-data-file", DEFAULT_ADMIN_DATA_FILE, "Path to the SQLite file used to store the administration history. If empty, it is stored next to -admin-password-file, or in the current directory.")
	flag.StringVar(&geoIPAPIURL, "geoip-api-url", DEFAULT_GEOIP_API_URL, "Base URL of the IP geolocation API used by the admin dashboard. Set it to empty to disable geolocation.")

	flag.Parse()

	if len(addresses) == 0 {
		addresses = make(AddressList, 1)
		addresses[0] = DEFAULT_ADDRESS
	}

	if AdminEnabled() && len(wsAddresses) == 0 {
		Log(LOG_INFO, "Warning: admin dashboard is enabled but no WebSocket listener (-ws-address) is configured, so the dashboard will not be reachable.")
	}

	c := cfg_default()
	cfg_err := c.Setup()

	log_init(logfile)

	Log(LOG_INFO, "Initializing configuration.")
	c.LogWrite()

	if c.panicString != "" {
		Log_close()
		os.Exit(2)
	}
	if cfg_err != nil {
		Log_close()
		os.Exit(1)
	}
	if AdminEnabled() {
		if adminDataFile == "" {
			if adminPasswordFile != "" {
				adminDataFile = filepath.Join(filepath.Dir(adminPasswordFile), "admin-history.db")
			} else {
				adminDataFile = "admin-history.db"
			}
		}
		startAdminHistory()
	}

	if !screenShare && (len(turnUrls) > 0 || !default_turn_secret(turnSecret) || !default_turn_secret_file(turnSecretFile)) {
		Log(LOG_INFO, "Warning: TURN parameters are set but screen sharing is disabled, so they will be ignored. Add -screen-share to enable it.")
	}
	if screenShare {
		if !default_turn_secret_file(turnSecretFile) {
			if err := turn_secret_load(turnSecretFile); err != nil {
				Log_error("Unable to read the TURN secret file at " + turnSecretFile + "\n" + err.Error())
			}
		}
		if len(turnUrls) == 0 {
			Log(LOG_INFO, "Warning: screen sharing is enabled but no STUN or TURN URL is configured. Clients will have to rely on a direct peer to peer connection, which fails on restrictive networks.")
		}
		if turn_needs_secret(turnUrls) && default_turn_secret(turnSecret) {
			Log(LOG_INFO, "Warning: a TURN URL is configured but no shared secret is set, so no credentials will be handed to clients and the TURN server will reject them.")
		}
		Log(LOG_INFO, "Screen sharing signaling is enabled. Media never transits through this server.")
	}

	defer PanicHandle.Catch()

	generate := false
	var config *tls.Config
	var err error

	if !default_cert_file(cert) && !fileExists(cert) {
		Log(LOG_INFO, "The certificate file at "+cert+" does not exist.")
		generate = true
	}
	if !default_key_file(key) && !fileExists(key) {
		Log(LOG_INFO, "The key file at "+key+" does not exist.")
		generate = true
	}
	if default_cert_file(cert) || default_key_file(key) {
		generate = true
	}

	if generate {
		Log(LOG_DEBUG, "Attempting to generate self-signed SSL certificate.")
		config, err = gen_cert()
		if err != nil {
			Log_error("Unable to generate self-signed certificate.\r\n" + err.Error() + "\r\nUnable to start server.")
			Launch_fail()
			return err
		}
		Log(LOG_DEBUG, "SSL certificate generated.")
	} else {
		if gencertfile != "" {
			Log(LOG_INFO, "The server has not generated its own self-signed certificate, and the -gen-certfile parameter is set to "+gencertfile+". This parameter will be ignored.")
		}
		cert, cerr := tls.LoadX509KeyPair(cert, key)
		if cerr != nil {
			Log_error("Error loading certificate and key files.\r\n" + cerr.Error() + "\r\nUnable to start server.")
			Launch_fail()
			return cerr
		}
		config = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
	}

	config.MinVersion = tls.VersionTLS12

	if loglevel < LOG_SILENT {
		loglevel = LOG_SILENT
		Log(LOG_INFO, "Log level is less than silent log value, resetting to "+strconv.Itoa(LOG_SILENT))
	}
	if loglevel > LOG_PROTOCOL {
		loglevel = LOG_PROTOCOL
		Log(LOG_INFO, "Log level is greater than protocol log value, resetting to "+strconv.Itoa(LOG_PROTOCOL))
	}

	if loglevel == LOG_PROTOCOL {
		Log(LOG_INFO, "Protocol logging is enabled. The server message of the day will be set to display always, and if unset, will have a value added to it that will alert all users connecting that protocol logging is enabled.")
		protocollogmotd := "WARNING!\nAll server information is being logged, including the protocol being used. This server is running in an insecure mode for production."
		if motd == "" {
			motd = protocollogmotd
		} else {
			motd = protocollogmotd + "\n" + motd
		}
		motdAlwaysDisplay = true
	}

	if !default_motd(motd) {
		logstr := "The server will display the following message of the day:\r\n" + motd
		if default_motd_always_display(motdAlwaysDisplay) {
			logstr += "\r\nThe server will tell each client to display this message of the day upon each connection."
		}
		Log(LOG_DEBUG, logstr)
	}

	if default_motd(motd) && !default_motd_always_display(motdAlwaysDisplay) {
		Log(LOG_INFO, "The server has been told to always display a message of the day, but no message of the day has been set. The -motd-always-display parameter will be reset to false.")
		motdAlwaysDisplay = false
	}

	if !sendOrigin {
		Log(LOG_INFO, "The server is configured to send no origin message to other clients, which may improve performance slightly, but impact the useability of your server when the origin field is required.")
	}

	if !Launch {
		Log(LOG_INFO, "The server will not be launched. Shutting down.")
		return errors.New("Server launch parameter set to false.")
	}

	Servers = make([]*Server, len(addresses))
	for i, addr := range addresses {
		Servers[i] = NewWithTLSConfig(addr, config)
		Log(LOG_DEBUG, "Starting server listening on address "+addr)
	}

	if wsPath == "" {
		wsPath = DEFAULT_WS_PATH
	}
	if !default_ws_raw(wsRaw) && len(wsAddresses) == 0 {
		Log(LOG_INFO, "Warning: the -ws-raw parameter is set but no WebSocket listener (-ws-address) is configured, so it will be ignored.")
	}
	for _, addr := range wsAddresses {
		Servers = append(Servers, NewWebSocketServer(addr, config, wsPath))
		logstr := "Starting WebSocket server listening on address " + addr + " at path " + wsPath
		if wsRaw {
			logstr += ", also accepting the historic NVDA Remote protocol on the same address"
		}
		Log(LOG_DEBUG, logstr)
	}

	return nil
}

func Start() int {
	num := 0
	var err error

	for i := range Servers {
		err = Servers[i].Listen()
		if err != nil {
			Log_error("Unable to listen on address " + Servers[i].address + ".\r\n" + err.Error())
			Servers[i] = nil
			continue
		}
		num++
	}
	if num == 0 {
		Servers = nil
		return num
	}

	Log(LOG_DEBUG, "Number of servers started: "+strconv.Itoa(num))
	go signals_init()
	return num
}

func Launch_fail() {
	if !Launch {
		os.Exit(1)
	}
}

func gen_conf_check() bool {
	return (!default_gen_conf_file(genConfFile) || !default_gen_conf_dir(genConfDir))
}
