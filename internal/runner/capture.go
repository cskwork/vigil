package runner

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// capture collects console/network/target events from every target the run
// attaches to. One capture is shared by the main page and any popup targets.
type capture struct {
	mu      sync.Mutex
	start   time.Time
	console []ConsoleEvent
	network []NetworkEvent
	reqIDs  []network.RequestID       // parallel to network (for Network.getResponseBody)
	byReqID map[network.RequestID]int // index into network

	// created receives page targets discovered while the run is active.
	created chan *target.Info

	sawConsole bool
	sawNetwork bool
}

func newCapture(start time.Time) *capture {
	return &capture{
		start:   start,
		byReqID: map[network.RequestID]int{},
		created: make(chan *target.Info, 32),
	}
}

func (c *capture) ms(t time.Time) int64 { return t.Sub(c.start).Milliseconds() }

// listenTarget registers page-level listeners on ctx. Call before the first
// chromedp.Run on that context so the listeners are attached with the target.
func (c *capture) listenTarget(ctx context.Context) {
	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			c.onConsole(e)
		case *runtime.EventExceptionThrown:
			c.onException(e)
		case *log.EventEntryAdded:
			c.onLogEntry(e)
		case *network.EventRequestWillBeSent:
			c.onRequest(e)
		case *network.EventResponseReceived:
			c.onResponse(e)
		case *network.EventLoadingFailed:
			c.onLoadingFailed(e)
		case *target.EventTargetCreated:
			c.onTargetCreated(e.TargetInfo)
		}
	})
}

// listenBrowser registers browser-level listeners (target discovery).
func (c *capture) listenBrowser(ctx context.Context) {
	chromedp.ListenBrowser(ctx, func(ev any) {
		if e, ok := ev.(*target.EventTargetCreated); ok {
			c.onTargetCreated(e.TargetInfo)
		}
	})
}

func (c *capture) onTargetCreated(info *target.Info) {
	if info == nil || info.Type != "page" {
		return
	}
	select {
	case c.created <- info:
	default:
	}
}

func consoleArgsText(args []*runtime.RemoteObject) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if a == nil {
			continue
		}
		switch {
		case a.Value != nil:
			parts = append(parts, strings.Trim(string(a.Value), `"`))
		case a.Description != "":
			parts = append(parts, a.Description)
		default:
			parts = append(parts, string(a.Type))
		}
	}
	return strings.Join(parts, " ")
}

func (c *capture) onConsole(e *runtime.EventConsoleAPICalled) {
	level := ""
	switch e.Type {
	case runtime.APITypeError, runtime.APITypeAssert:
		level = "error"
	case runtime.APITypeWarning:
		level = "warn"
	default:
		return // log/info/debug are not signals (PRD §13 console/runtime signals)
	}
	ev := ConsoleEvent{Level: level, Source: "console", Text: consoleArgsText(e.Args), At: c.ms(time.Now())}
	if e.StackTrace != nil && len(e.StackTrace.CallFrames) > 0 {
		f := e.StackTrace.CallFrames[0]
		ev.URL, ev.Line = f.URL, int(f.LineNumber)
	}
	c.mu.Lock()
	c.sawConsole = true
	c.console = append(c.console, ev)
	c.mu.Unlock()
}

func (c *capture) onException(e *runtime.EventExceptionThrown) {
	d := e.ExceptionDetails
	if d == nil {
		return
	}
	text := d.Text
	if d.Exception != nil && d.Exception.Description != "" {
		text = d.Exception.Description
	}
	ev := ConsoleEvent{Level: "exception", Source: "exception", Text: text, URL: d.URL, Line: int(d.LineNumber), At: c.ms(time.Now())}
	c.mu.Lock()
	c.sawConsole = true
	c.console = append(c.console, ev)
	c.mu.Unlock()
}

func (c *capture) onLogEntry(e *log.EventEntryAdded) {
	if e.Entry == nil {
		return
	}
	level := ""
	switch e.Entry.Level {
	case log.LevelError:
		level = "error"
	case log.LevelWarning:
		level = "warn"
	default:
		return
	}
	// Browser log entries (e.g. source=network "Failed to load resource") duplicate
	// network.json; keep them but mark the source so the console oracle can skip them.
	c.mu.Lock()
	c.console = append(c.console, ConsoleEvent{Level: level, Source: "log:" + string(e.Entry.Source), Text: e.Entry.Text, URL: e.Entry.URL, Line: int(e.Entry.LineNumber), At: c.ms(time.Now())})
	c.mu.Unlock()
}

func (c *capture) onRequest(e *network.EventRequestWillBeSent) {
	if e.Request == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sawNetwork = true
	if i, ok := c.byReqID[e.RequestID]; ok {
		// redirect: the same request id is re-sent with a new URL
		c.network[i].URL = e.Request.URL
		c.network[i].Method = e.Request.Method
		return
	}
	c.byReqID[e.RequestID] = len(c.network)
	c.network = append(c.network, NetworkEvent{Method: e.Request.Method, URL: e.Request.URL, At: c.ms(time.Now())})
	c.reqIDs = append(c.reqIDs, e.RequestID)
}

func (c *capture) onResponse(e *network.EventResponseReceived) {
	if e.Response == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sawNetwork = true
	i, ok := c.byReqID[e.RequestID]
	if !ok {
		c.byReqID[e.RequestID] = len(c.network)
		c.network = append(c.network, NetworkEvent{URL: e.Response.URL, At: c.ms(time.Now())})
		c.reqIDs = append(c.reqIDs, e.RequestID)
		i = len(c.network) - 1
	}
	c.network[i].Status = int(e.Response.Status)
	c.network[i].MimeType = e.Response.MimeType
}

func (c *capture) onLoadingFailed(e *network.EventLoadingFailed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sawNetwork = true
	i, ok := c.byReqID[e.RequestID]
	if !ok {
		c.byReqID[e.RequestID] = len(c.network)
		c.network = append(c.network, NetworkEvent{At: c.ms(time.Now())})
		c.reqIDs = append(c.reqIDs, e.RequestID)
		i = len(c.network) - 1
	}
	c.network[i].Failed = true
	c.network[i].Error = e.ErrorText
	if e.Canceled {
		c.network[i].Error = strings.TrimSpace(e.ErrorText + " (canceled)")
	}
}

// findRequest returns the first captured request accepted by pred, with its CDP id.
func (c *capture) findRequest(pred func(NetworkEvent) bool) (NetworkEvent, network.RequestID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, ev := range c.network {
		if pred(ev) {
			return ev, c.reqIDs[i], true
		}
	}
	return NetworkEvent{}, "", false
}

// findLastRequest returns the most recent captured request accepted by pred.
func (c *capture) findLastRequest(pred func(NetworkEvent) bool) (NetworkEvent, network.RequestID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.network) - 1; i >= 0; i-- {
		if pred(c.network[i]) {
			return c.network[i], c.reqIDs[i], true
		}
	}
	return NetworkEvent{}, "", false
}

// snapshotNetwork returns a copy of the captured network events.
func (c *capture) snapshotNetwork() []NetworkEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]NetworkEvent, len(c.network))
	copy(out, c.network)
	return out
}

func (c *capture) snapshotConsole() []ConsoleEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ConsoleEvent, len(c.console))
	copy(out, c.console)
	return out
}

func (c *capture) consoleSeen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sawConsole
}

func (c *capture) networkSeen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sawNetwork
}

// drainCreated empties the popup discovery channel (called before an action
// that is expected to open a popup so stale targets are not mistaken for it).
func (c *capture) drainCreated() {
	for {
		select {
		case <-c.created:
		default:
			return
		}
	}
}
