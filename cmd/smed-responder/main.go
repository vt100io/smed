// smed-responder answers one question over HTTP: what does this machine look
// like right now? GET / returns a machine.Report as JSON. It is read-only by
// construction: it takes no input but the path, accepts only GET and writes
// nothing. It runs where unit puts it, behind a systemd socket on the
// tailnet, and starts when someone first asks.
package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/vt100io/smed/watch/machine"
)

func main() {
	ln, err := listen()
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Handler:           handler(machine.Live()),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	notifySystemd("READY=1")
	go watchdog()
	slog.Info("answering", "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

func handler(r machine.Reader) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "smed only answers questions", http.StatusMethodNotAllowed)
			return
		}
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		rep, err := r.Read()
		if err != nil {
			slog.Error("read machine", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store") // the answer is about now
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	})
	return mux
}

// listen adopts the socket systemd opened for this service (socket
// activation, as unit sets it up), or binds $PORT (default machine.Port) when run
// by hand.
func listen() (net.Listener, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1" {
		return net.FileListener(os.NewFile(3, "systemd-socket"))
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = machine.Port
	}
	return net.Listen("tcp", "localhost:"+port)
}

// notifySystemd sends state to systemd's notify socket. It does nothing
// outside systemd.
func notifySystemd(state string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return
	}
	conn, err := net.Dial("unixgram", socket)
	if err != nil {
		slog.Warn("systemd notify", "err", err)
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(state))
}

// watchdog pings systemd at half the interval it asked for.
func watchdog() {
	usec, err := strconv.Atoi(os.Getenv("WATCHDOG_USEC"))
	if err != nil || usec <= 0 {
		return
	}
	for {
		notifySystemd("WATCHDOG=1")
		time.Sleep(time.Duration(usec) * time.Microsecond / 2)
	}
}
