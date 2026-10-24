package domainchecker

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	clientmanager "ethanstewart.net/domainchecker/server/clientmanager"
	"github.com/gorilla/websocket"
)

// requestMaker is the subset of RequestRegistry that DomainCheckerService
// needs to classify a URL, kept narrow so tests can substitute a fake
// decision instead of a real RequestRegistry/ClientManager/websocket
// stack. *clientmanager.RequestRegistry satisfies it already.
type requestMaker interface {
	MakeRequest(url url.URL) (bool, string)
}

// DomainCheckerService implements Squid's external_acl_type helper
// protocol: it reads one classification request per line from stdin, asks
// a connected websocket client to approve or deny it via requestRegister,
// and writes Squid's expected OK/ERR/BH response to stdout.
type DomainCheckerService struct {
	clientManager     *clientmanager.ClientManager
	requestRegister   requestMaker
	handleStdioMu     *sync.Mutex
	websocketUpgrader websocket.Upgrader
}

// NewDomainCheckerService constructs a ready-to-use service, including its
// own ClientManager and RequestRegistry, and starts the registry's request
// dispatch loops. Start must still be called separately to begin servicing
// stdin.
func NewDomainCheckerService() *DomainCheckerService {
	dcs := DomainCheckerService{
		clientManager:     clientmanager.NewClientManager(),
		requestRegister:   nil,
		handleStdioMu:     &sync.Mutex{},
		websocketUpgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}
	registry := dcs.clientManager.NewRequestRegistry()
	dcs.requestRegister = registry
	go registry.Start()
	return &dcs
}

// HandleWebsocket upgrades an incoming HTTP request to a websocket
// connection and hands it to the ClientManager. CheckOrigin unconditionally
// accepts every origin, so this must only be exposed on a trusted network.
func (s *DomainCheckerService) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.websocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("Upgrading error", "error", err)
		return
	}
	s.clientManager.AddClient(conn)
}

// Close tears down the ClientManager and every client connected through
// it. It does not stop handleStdio; that loop only ends on its own when
// stdin reaches EOF or errors.
func (s *DomainCheckerService) Close() {
	s.clientManager.Close()
}

// Start begins servicing the Squid helper protocol on a background
// goroutine and returns immediately: requests are read line-by-line from
// stdin, and OK/ERR/BH responses are written to stdout -- normally
// os.Stdin/os.Stdout, but callers (tests, in particular) can supply any
// io.Reader/io.Writer to drive the protocol without a real Squid process.
// Must be called once per service instance: handleStdioMu prevents two
// scanners from running at once, but a second call would simply block
// forever waiting for the first to finish, rather than doing anything
// useful.
func (s *DomainCheckerService) Start(stdin io.Reader, stdout io.Writer) {
	go s.handleStdio(stdin, stdout)
}

// makeRequest blocks until requestRegister resolves url: approved, denied,
// or (if no client was ever connected) rejected immediately. The second
// return value is a human-readable reason for a denial, or "" on approval;
// see RequestRegistry.MakeRequest.
func (s *DomainCheckerService) makeRequest(url url.URL) (bool, string) {
	return s.requestRegister.MakeRequest(url)
}

// handleStdio implements Squid's external_acl_type line protocol: each
// line read from stdin is "<client-ip> <encoded-url> ...", and this blocks
// until EOF or a read error, running one classification through
// makeRequest per line in order. Writes OK or ERR to stdout for a line
// that was parsed, or BH for one that couldn't be parsed at all, per the
// format Squid's helper protocol expects. A denial carrying a non-empty
// reason is written as "ERR message=<quoted reason>", so Squid can expose
// it to an error page via %o (see README). Must run on its own goroutine,
// and handleStdioMu ensures only one instance is ever running at a time.
func (s *DomainCheckerService) handleStdio(stdin io.Reader, stdout io.Writer) {
	s.handleStdioMu.Lock()
	defer s.handleStdioMu.Unlock()

	scanner := bufio.NewScanner(stdin)

	for scanner.Scan() {
		line := scanner.Text()

		parts := strings.Fields(line)
		if len(parts) < 2 {
			fmt.Fprintln(stdout, "ERR message=Invalid_Input_Format")
			slog.Warn("Failed to parse request", "line", line)
			continue
		}

		encodedURI := parts[1]
		if !strings.Contains(encodedURI, "://") {
			// Squid's %URI for a CONNECT request -- which is how every HTTPS
			// request reaches this helper, since Squid can't see past the
			// TLS tunnel without SSL-bumping it -- is bare "host:port", with
			// no scheme. url.Parse doesn't error on that, but silently
			// mis-parses it (Scheme=host, Opaque=port, Host empty), which
			// leaves Hostname() returning "" for every HTTPS target and
			// collapses domain-scoped memory (decisionDomain) across all of
			// them. CONNECT only ever sets up a TLS tunnel, so treating it
			// as https gives url.Parse a well-formed URL instead.
			encodedURI = "https://" + encodedURI
		}

		parsedUrl, err := url.Parse(encodedURI)
		if err != nil {
			fmt.Fprintln(stdout, "BH message=URL_Decoding_Failed")
			slog.Warn("Failed to parse request", "line", line)
			continue
		}

		allowed, denyMessage := s.makeRequest(*parsedUrl)
		switch {
		case allowed:
			fmt.Fprintln(stdout, "OK")
		case denyMessage != "":
			fmt.Fprintf(stdout, "ERR message=%s\n", quoteSquidValue(denyMessage))
		default:
			fmt.Fprintln(stdout, "ERR")
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Error("Error reading standard input", "error", err)
	}
}

// quoteSquidValue renders s as a double-quoted value for a keyword=value
// pair in Squid's external_acl_type helper response line, escaping
// backslashes and double quotes and collapsing any embedded newline --
// which would otherwise desync the line-based protocol -- into a space.
func quoteSquidValue(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")

	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
