package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// replyLine is one line of a BIRD control-socket reply with its 4-digit code.
type replyLine struct {
	Code string
	Text string
}

type birdError struct {
	Code string
	Msg  string
}

func (e *birdError) Error() string { return "BIRD " + e.Code + ": " + e.Msg }

func isNotFound(err error) bool {
	var be *birdError
	return errors.As(err, &be) && be.Code == "8001"
}

type birdClient struct {
	socket  string
	timeout time.Duration
}

// run opens a fresh connection, switches it to restricted mode (read-only
// show commands) and executes exactly one command.
func (b *birdClient) run(cmd string, timeout time.Duration) ([]replyLine, error) {
	if timeout == 0 {
		timeout = b.timeout
	}
	conn, err := net.DialTimeout("unix", b.socket, timeout)
	if err != nil {
		return nil, fmt.Errorf("BIRD-Socket %s nicht erreichbar: %w", b.socket, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	r := bufio.NewReaderSize(conn, 64*1024)

	if _, err := readReply(r); err != nil { // banner "0001 BIRD x.y ready."
		return nil, err
	}
	if _, err := fmt.Fprint(conn, "restrict\n"); err != nil {
		return nil, err
	}
	lines, err := readReply(r)
	if err != nil {
		return nil, fmt.Errorf("restrict fehlgeschlagen: %w", err)
	}
	if len(lines) == 0 || lines[len(lines)-1].Code != "0016" {
		return nil, errors.New("BIRD hat den Restricted-Modus nicht bestätigt")
	}
	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return nil, err
	}
	return readReply(r)
}

func isCode(s string) bool {
	if len(s) < 5 || (s[4] != '-' && s[4] != ' ') {
		return false
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// readReply reads until the final line ("CCCC text"). Error codes 8xxx/9xxx
// are returned as *birdError together with the lines read so far.
func readReply(r *bufio.Reader) ([]replyLine, error) {
	var out []replyLine
	last := ""
	for {
		s, err := r.ReadString('\n')
		if err != nil {
			return out, fmt.Errorf("Lesen vom BIRD-Socket: %w", err)
		}
		s = strings.TrimRight(s, "\r\n")
		switch {
		case isCode(s):
			code, text := s[:4], s[5:]
			last = code
			if s[4] == ' ' {
				if code[0] == '8' || code[0] == '9' {
					return out, &birdError{Code: code, Msg: text}
				}
				if text != "" || code != "0000" {
					out = append(out, replyLine{code, text})
				}
				return out, nil
			}
			out = append(out, replyLine{code, text})
		case strings.HasPrefix(s, " "):
			out = append(out, replyLine{last, s[1:]})
		default:
			out = append(out, replyLine{last, s})
		}
	}
}

func joinText(lines []replyLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// ---- data model -----------------------------------------------------------

type Status struct {
	Version      string `json:"version"`
	RouterID     string `json:"routerId"`
	ServerTime   string `json:"serverTime"`
	LastReboot   string `json:"lastReboot"`
	LastReconfig string `json:"lastReconfig"`
}

type Channel struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Table      string `json:"table"`
	Imported   int64  `json:"imported"`
	Exported   int64  `json:"exported"`
	Preferred  int64  `json:"preferred"`
	Filtered   int64  `json:"filtered"`
	ImpUpdRecv int64  `json:"-"`
	ImpWdrRecv int64  `json:"-"`
	ExpUpdAcc  int64  `json:"-"`
	ExpWdrAcc  int64  `json:"-"`
}

type Protocol struct {
	Name         string    `json:"name"`
	Proto        string    `json:"proto"`
	Table        string    `json:"table"`
	State        string    `json:"state"`
	Since        string    `json:"since"`
	Info         string    `json:"info"`
	Description  string    `json:"description,omitempty"`
	BGPState     string    `json:"bgpState,omitempty"`
	NeighborAddr string    `json:"neighborAddr,omitempty"`
	NeighborAS   uint64    `json:"neighborAs,omitempty"`
	LocalAS      uint64    `json:"localAs,omitempty"`
	Kind         string    `json:"kind"` // ebgp | ibgp | other
	Channels     []Channel `json:"channels"`
	RateIn       float64   `json:"rateIn"`
	RateOut      float64   `json:"rateOut"`
	HistIn       []float64 `json:"histIn"`
	HistOut      []float64 `json:"histOut"`
}

// ---- parsers --------------------------------------------------------------

var reTimeTok = regexp.MustCompile(`^\d{1,2}:\d{2}(:\d{2})?(\.\d+)?$`)
var reDateTok = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
var reCount = regexp.MustCompile(`(\d+)\s+([a-z]+)`)

func parseHeader(text string) Protocol {
	f := strings.Fields(text)
	p := Protocol{Kind: "other"}
	if len(f) < 4 {
		if len(f) > 0 {
			p.Name = f[0]
		}
		return p
	}
	p.Name, p.Proto, p.Table, p.State = f[0], f[1], f[2], f[3]
	rest := f[4:]
	if len(rest) > 0 {
		p.Since = rest[0]
		rest = rest[1:]
		// "2026-09-30 12:00:00" style time formats
		if reDateTok.MatchString(p.Since) && len(rest) > 0 && reTimeTok.MatchString(rest[0]) {
			p.Since += " " + rest[0]
			rest = rest[1:]
		}
	}
	p.Info = strings.Join(rest, " ")
	return p
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func afterColon(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return ""
}

// parseProtocols parses the reply of "show protocols all".
func parseProtocols(lines []replyLine) []Protocol {
	var out []Protocol
	var cur *Protocol
	var ch *Channel
	var statCols []string

	flushCh := func() {
		if cur != nil && ch != nil {
			cur.Channels = append(cur.Channels, *ch)
		}
		ch = nil
	}
	flush := func() {
		flushCh()
		if cur != nil {
			if cur.Proto == "BGP" {
				if cur.NeighborAS != 0 && cur.NeighborAS == cur.LocalAS {
					cur.Kind = "ibgp"
				} else {
					cur.Kind = "ebgp"
				}
			}
			out = append(out, *cur)
		}
		cur = nil
	}

	for _, l := range lines {
		switch l.Code {
		case "1002":
			flush()
			p := parseHeader(l.Text)
			cur = &p
			continue
		case "1006":
		default:
			continue
		}
		if cur == nil {
			continue
		}
		t := strings.TrimSpace(l.Text)
		switch {
		case t == "":
		case strings.HasPrefix(t, "Channel "):
			flushCh()
			ch = &Channel{Name: strings.TrimSpace(strings.TrimPrefix(t, "Channel "))}
		case strings.HasPrefix(t, "Description:"):
			cur.Description = afterColon(t)
		case strings.HasPrefix(t, "BGP state:"):
			cur.BGPState = afterColon(t)
		case strings.HasPrefix(t, "Neighbor address:"):
			cur.NeighborAddr = afterColon(t)
		case strings.HasPrefix(t, "Neighbor AS:"):
			cur.NeighborAS = uint64(atoi(afterColon(t)))
		case strings.HasPrefix(t, "Local AS:"):
			cur.LocalAS = uint64(atoi(afterColon(t)))
		case ch != nil && strings.HasPrefix(t, "State:"):
			ch.State = afterColon(t)
		case ch != nil && strings.HasPrefix(t, "Table:"):
			ch.Table = afterColon(t)
		case ch != nil && strings.HasPrefix(t, "Routes:"):
			for _, m := range reCount.FindAllStringSubmatch(afterColon(t), -1) {
				n := atoi(m[1])
				switch m[2] {
				case "imported":
					ch.Imported = n
				case "exported":
					ch.Exported = n
				case "preferred":
					ch.Preferred = n
				case "filtered":
					ch.Filtered = n
				}
			}
		case ch != nil && strings.HasPrefix(t, "Route change stats:"):
			h := strings.ReplaceAll(afterColon(t), "RX limit", "rxlimit")
			statCols = strings.Fields(h)
		case ch != nil && (strings.HasPrefix(t, "Import updates:") ||
			strings.HasPrefix(t, "Import withdraws:") ||
			strings.HasPrefix(t, "Export updates:") ||
			strings.HasPrefix(t, "Export withdraws:")):
			vals := strings.Fields(afterColon(t))
			get := func(col string) int64 {
				for i, c := range statCols {
					if c == col && i < len(vals) {
						return atoi(vals[i])
					}
				}
				return 0
			}
			switch {
			case strings.HasPrefix(t, "Import updates:"):
				ch.ImpUpdRecv = get("received")
			case strings.HasPrefix(t, "Import withdraws:"):
				ch.ImpWdrRecv = get("received")
			case strings.HasPrefix(t, "Export updates:"):
				ch.ExpUpdAcc = get("accepted")
			case strings.HasPrefix(t, "Export withdraws:"):
				ch.ExpWdrAcc = get("accepted")
			}
		}
	}
	flush()
	return out
}

func parseStatus(lines []replyLine) Status {
	var s Status
	for _, l := range lines {
		t := strings.TrimSpace(l.Text)
		switch {
		case strings.HasPrefix(t, "BIRD "):
			s.Version = strings.TrimPrefix(t, "BIRD ")
		case strings.HasPrefix(t, "Router ID is "):
			s.RouterID = strings.TrimPrefix(t, "Router ID is ")
		case strings.HasPrefix(t, "Current server time is "):
			s.ServerTime = strings.TrimPrefix(t, "Current server time is ")
		case strings.HasPrefix(t, "Last reboot on "):
			s.LastReboot = strings.TrimPrefix(t, "Last reboot on ")
		case strings.HasPrefix(t, "Last reconfiguration on "):
			s.LastReconfig = strings.TrimPrefix(t, "Last reconfiguration on ")
		}
	}
	return s
}
