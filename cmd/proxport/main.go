package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultDialTimeout    = 5 * time.Second
	defaultShutdownWindow = 10 * time.Second
	defaultUDPIdleTimeout = 60 * time.Second
	udpReadBufferSize     = 64 * 1024
)

type Config struct {
	ListenAddress string        `json:"listen_address"`
	DialTimeout   Duration      `json:"dial_timeout"`
	Forwards      []ForwardRule `json:"forwards"`
}

type ForwardRule struct {
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	ListenPort int    `json:"listen_port"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}

	d.Duration = parsed
	return nil
}

type App struct {
	cfg       Config
	logger    *log.Logger
	listeners []io.Closer
	wg        sync.WaitGroup
}

type udpSession struct {
	clientAddr net.Addr
	upstream   net.Conn
	lastSeen   time.Time
	mu         sync.Mutex
}

func (s *udpSession) touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

func (s *udpSession) seenAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeen
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)

	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		logger.Fatalf("failed to load config: %v", err)
	}

	app := &App{
		cfg:    cfg,
		logger: logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatalf("proxy exited with error: %v", err)
	}
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	switch strings.ToLower(filepathExt(path)) {
	case ".yaml", ".yml":
		cfg, err = parseYAMLConfig(data)
		if err != nil {
			return Config{}, fmt.Errorf("parse YAML config: %w", err)
		}
	default:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse JSON config: %w", err)
		}
	}

	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "0.0.0.0"
	}
	if cfg.DialTimeout.Duration == 0 {
		cfg.DialTimeout.Duration = defaultDialTimeout
	}

	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func parseYAMLConfig(data []byte) (Config, error) {
	var cfg Config
	var currentRule *ForwardRule
	inForwards := false

	lines := strings.Split(string(data), "\n")
	for index, rawLine := range lines {
		lineNumber := index + 1
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.ContainsRune(line, '\t') {
			return Config{}, fmt.Errorf("line %d: tabs are not supported in YAML config", lineNumber)
		}

		indent := len(line) - len(strings.TrimLeft(line, " "))
		if strings.HasPrefix(trimmed, "- ") {
			if !inForwards {
				return Config{}, fmt.Errorf("line %d: list item is only valid under forwards", lineNumber)
			}
			if indent != 2 {
				return Config{}, fmt.Errorf("line %d: forward entries must be indented by two spaces", lineNumber)
			}

			cfg.Forwards = append(cfg.Forwards, ForwardRule{})
			currentRule = &cfg.Forwards[len(cfg.Forwards)-1]

			remainder := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			if remainder == "" {
				continue
			}

			key, value, err := splitYAMLKeyValue(remainder, lineNumber)
			if err != nil {
				return Config{}, err
			}
			if err := assignForwardField(currentRule, key, value, lineNumber); err != nil {
				return Config{}, err
			}
			continue
		}

		key, value, err := splitYAMLKeyValue(trimmed, lineNumber)
		if err != nil {
			return Config{}, err
		}

		switch indent {
		case 0:
			currentRule = nil
			if value == "" {
				if key != "forwards" {
					return Config{}, fmt.Errorf("line %d: nested mappings are only supported for forwards", lineNumber)
				}
				inForwards = true
				continue
			}

			inForwards = false
			if err := assignConfigField(&cfg, key, value, lineNumber); err != nil {
				return Config{}, err
			}
		case 4:
			if !inForwards || currentRule == nil {
				return Config{}, fmt.Errorf("line %d: unexpected nested field", lineNumber)
			}
			if err := assignForwardField(currentRule, key, value, lineNumber); err != nil {
				return Config{}, err
			}
		default:
			return Config{}, fmt.Errorf("line %d: unsupported indentation level %d", lineNumber, indent)
		}
	}

	return cfg, nil
}

func splitYAMLKeyValue(line string, lineNumber int) (string, string, error) {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("line %d: expected key: value", lineNumber)
	}

	key := strings.TrimSpace(parts[0])
	value := strings.TrimSpace(parts[1])
	if key == "" {
		return "", "", fmt.Errorf("line %d: empty key", lineNumber)
	}

	return key, unquoteYAMLValue(value), nil
}

func unquoteYAMLValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func assignConfigField(cfg *Config, key, value string, lineNumber int) error {
	switch key {
	case "listen_address":
		cfg.ListenAddress = value
	case "dial_timeout":
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("line %d: invalid dial_timeout %q: %w", lineNumber, value, err)
		}
		cfg.DialTimeout.Duration = duration
	default:
		return fmt.Errorf("line %d: unknown config key %q", lineNumber, key)
	}
	return nil
}

func assignForwardField(rule *ForwardRule, key, value string, lineNumber int) error {
	switch key {
	case "name":
		rule.Name = value
	case "protocol":
		rule.Protocol = normalizedProtocol(value)
	case "listen_port":
		port, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("line %d: invalid listen_port %q", lineNumber, value)
		}
		rule.ListenPort = port
	case "target_host":
		rule.TargetHost = value
	case "target_port":
		port, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("line %d: invalid target_port %q", lineNumber, value)
		}
		rule.TargetPort = port
	default:
		return fmt.Errorf("line %d: unknown forward key %q", lineNumber, key)
	}
	return nil
}

func filepathExt(path string) string {
	lastDot := strings.LastIndex(path, ".")
	if lastDot == -1 {
		return ""
	}
	return path[lastDot:]
}

func validateConfig(cfg Config) error {
	if ip := net.ParseIP(cfg.ListenAddress); ip == nil && cfg.ListenAddress != "0.0.0.0" && cfg.ListenAddress != "::" {
		return fmt.Errorf("listen_address must be a valid IP, got %q", cfg.ListenAddress)
	}
	if len(cfg.Forwards) == 0 {
		return errors.New("config must contain at least one forward rule")
	}

	usedPorts := map[string]string{}
	for i := range cfg.Forwards {
		rule := &cfg.Forwards[i]
		rule.Protocol = normalizedProtocol(rule.Protocol)

		if rule.Protocol != "tcp" && rule.Protocol != "udp" {
			return fmt.Errorf("forwards[%d].protocol must be tcp or udp", i)
		}
		if rule.ListenPort < 1 || rule.ListenPort > 65535 {
			return fmt.Errorf("forwards[%d].listen_port must be between 1 and 65535", i)
		}
		if rule.TargetPort < 1 || rule.TargetPort > 65535 {
			return fmt.Errorf("forwards[%d].target_port must be between 1 and 65535", i)
		}
		if net.ParseIP(rule.TargetHost) == nil {
			return fmt.Errorf("forwards[%d].target_host must be a valid IP address", i)
		}

		portKey := fmt.Sprintf("%s:%d", rule.Protocol, rule.ListenPort)
		if previous, exists := usedPorts[portKey]; exists {
			return fmt.Errorf("%s listen_port %d is used by both %q and %q", rule.Protocol, rule.ListenPort, previous, rule.Name)
		}

		name := rule.Name
		if name == "" {
			name = fmt.Sprintf("rule-%d", i+1)
		}
		usedPorts[portKey] = name
	}

	return nil
}

func (a *App) Run(ctx context.Context) error {
	for _, rule := range a.cfg.Forwards {
		switch rule.Protocol {
		case "tcp":
			if err := a.startTCPRule(ctx, rule); err != nil {
				a.closeListeners()
				return err
			}
		case "udp":
			if err := a.startUDPRule(ctx, rule); err != nil {
				a.closeListeners()
				return err
			}
		default:
			a.closeListeners()
			return fmt.Errorf("unsupported protocol %q for rule %q", rule.Protocol, displayName(rule))
		}
	}

	<-ctx.Done()
	a.logger.Printf("shutdown signal received, closing listeners")
	a.closeListeners()

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.wg.Wait()
	}()

	select {
	case <-done:
		a.logger.Printf("shutdown complete")
		return nil
	case <-time.After(defaultShutdownWindow):
		return errors.New("shutdown timed out while waiting for active handlers")
	}
}

func (a *App) startTCPRule(ctx context.Context, rule ForwardRule) error {
	addr := net.JoinHostPort(a.cfg.ListenAddress, strconv.Itoa(rule.ListenPort))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s for rule %q: %w", addr, displayName(rule), err)
	}

	a.listeners = append(a.listeners, listener)
	a.logger.Printf("listening on tcp %s -> %s", listener.Addr(), targetAddress(rule))

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.serveTCPListener(ctx, listener, rule)
	}()

	return nil
}

func (a *App) startUDPRule(ctx context.Context, rule ForwardRule) error {
	addr := net.JoinHostPort(a.cfg.ListenAddress, strconv.Itoa(rule.ListenPort))
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s for rule %q: %w", addr, displayName(rule), err)
	}

	a.listeners = append(a.listeners, conn)
	a.logger.Printf("listening on udp %s -> %s", conn.LocalAddr(), targetAddress(rule))

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.serveUDPListener(ctx, conn, rule)
	}()

	return nil
}

func (a *App) serveTCPListener(ctx context.Context, listener net.Listener, rule ForwardRule) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if isClosedNetworkError(err) {
				return
			}

			a.logger.Printf("accept failed for %q: %v", displayName(rule), err)
			continue
		}

		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.handleTCPConnection(conn, rule)
		}()
	}
}

func (a *App) serveUDPListener(ctx context.Context, conn net.PacketConn, rule ForwardRule) {
	buffer := make([]byte, udpReadBufferSize)
	sessions := make(map[string]*udpSession)
	var sessionsMu sync.Mutex

	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, clientAddr, err := conn.ReadFrom(buffer)
		if err != nil {
			if ctx.Err() != nil || isClosedNetworkError(err) {
				closeAllUDPSessions(&sessionsMu, sessions)
				return
			}

			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				cleanupUDPSessions(&sessionsMu, sessions)
				continue
			}

			a.logger.Printf("udp read failed for %q: %v", displayName(rule), err)
			continue
		}

		cleanupUDPSessions(&sessionsMu, sessions)

		session, err := a.getOrCreateUDPSession(conn, clientAddr, rule, &sessionsMu, sessions)
		if err != nil {
			a.logger.Printf("udp upstream connect failed for %q: %v", displayName(rule), err)
			continue
		}

		session.touch()
		_ = session.upstream.SetWriteDeadline(time.Now().Add(a.cfg.DialTimeout.Duration))
		if _, err := session.upstream.Write(buffer[:n]); err != nil {
			a.logger.Printf("udp upstream write failed for %q: %v", displayName(rule), err)
			removeUDPSession(&sessionsMu, sessions, clientAddr.String())
			continue
		}
	}
}

func (a *App) handleTCPConnection(client net.Conn, rule ForwardRule) {
	defer client.Close()

	target := targetAddress(rule)
	upstream, err := net.DialTimeout("tcp", target, a.cfg.DialTimeout.Duration)
	if err != nil {
		a.logger.Printf("connect failed for %q to %s: %v", displayName(rule), target, err)
		return
	}
	defer upstream.Close()

	a.logger.Printf("%q connected %s -> %s", displayName(rule), client.RemoteAddr(), target)

	var copyWG sync.WaitGroup
	copyWG.Add(2)

	go func() {
		defer copyWG.Done()
		_, _ = io.Copy(upstream, client)
		_ = upstream.SetDeadline(time.Now())
	}()

	go func() {
		defer copyWG.Done()
		_, _ = io.Copy(client, upstream)
		_ = client.SetDeadline(time.Now())
	}()

	copyWG.Wait()
	a.logger.Printf("%q disconnected %s", displayName(rule), client.RemoteAddr())
}

func (a *App) getOrCreateUDPSession(
	downstream net.PacketConn,
	clientAddr net.Addr,
	rule ForwardRule,
	sessionsMu *sync.Mutex,
	sessions map[string]*udpSession,
) (*udpSession, error) {
	key := clientAddr.String()

	sessionsMu.Lock()
	session, ok := sessions[key]
	sessionsMu.Unlock()
	if ok {
		return session, nil
	}

	upstream, err := net.DialTimeout("udp", targetAddress(rule), a.cfg.DialTimeout.Duration)
	if err != nil {
		return nil, err
	}

	session = &udpSession{
		clientAddr: clientAddr,
		upstream:   upstream,
		lastSeen:   time.Now(),
	}

	sessionsMu.Lock()
	if existing, exists := sessions[key]; exists {
		sessionsMu.Unlock()
		_ = upstream.Close()
		return existing, nil
	}
	sessions[key] = session
	sessionsMu.Unlock()

	a.logger.Printf("%q udp session %s -> %s", displayName(rule), clientAddr.String(), targetAddress(rule))

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.relayUDPResponses(downstream, session, rule, sessionsMu, sessions)
	}()

	return session, nil
}

func (a *App) relayUDPResponses(
	downstream net.PacketConn,
	session *udpSession,
	rule ForwardRule,
	sessionsMu *sync.Mutex,
	sessions map[string]*udpSession,
) {
	defer removeUDPSession(sessionsMu, sessions, session.clientAddr.String())

	buffer := make([]byte, udpReadBufferSize)
	for {
		_ = session.upstream.SetReadDeadline(time.Now().Add(defaultUDPIdleTimeout))
		n, err := session.upstream.Read(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if time.Since(session.seenAt()) >= defaultUDPIdleTimeout {
					break
				}
				continue
			}
			if !isClosedNetworkError(err) {
				a.logger.Printf("udp upstream read failed for %q: %v", displayName(rule), err)
			}
			break
		}

		session.touch()
		if _, err := downstream.WriteTo(buffer[:n], session.clientAddr); err != nil {
			if !isClosedNetworkError(err) {
				a.logger.Printf("udp downstream write failed for %q: %v", displayName(rule), err)
			}
			break
		}
	}

	a.logger.Printf("%q udp session closed %s", displayName(rule), session.clientAddr.String())
}

func (a *App) closeListeners() {
	for _, listener := range a.listeners {
		_ = listener.Close()
	}
}

func displayName(rule ForwardRule) string {
	if strings.TrimSpace(rule.Name) != "" {
		return rule.Name
	}
	return fmt.Sprintf("%s-%d", normalizedProtocol(rule.Protocol), rule.ListenPort)
}

func normalizedProtocol(value string) string {
	if strings.TrimSpace(value) == "" {
		return "tcp"
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func targetAddress(rule ForwardRule) string {
	return net.JoinHostPort(rule.TargetHost, strconv.Itoa(rule.TargetPort))
}

func isClosedNetworkError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "use of closed network connection")
}

func cleanupUDPSessions(sessionsMu *sync.Mutex, sessions map[string]*udpSession) {
	var staleKeys []string

	sessionsMu.Lock()
	for key, session := range sessions {
		if time.Since(session.seenAt()) >= defaultUDPIdleTimeout {
			staleKeys = append(staleKeys, key)
		}
	}
	sessionsMu.Unlock()

	for _, key := range staleKeys {
		removeUDPSession(sessionsMu, sessions, key)
	}
}

func closeAllUDPSessions(sessionsMu *sync.Mutex, sessions map[string]*udpSession) {
	sessionsMu.Lock()
	keys := make([]string, 0, len(sessions))
	for key := range sessions {
		keys = append(keys, key)
	}
	sessionsMu.Unlock()

	for _, key := range keys {
		removeUDPSession(sessionsMu, sessions, key)
	}
}

func removeUDPSession(sessionsMu *sync.Mutex, sessions map[string]*udpSession, key string) {
	sessionsMu.Lock()
	session, ok := sessions[key]
	if ok {
		delete(sessions, key)
	}
	sessionsMu.Unlock()

	if ok {
		_ = session.upstream.Close()
	}
}
