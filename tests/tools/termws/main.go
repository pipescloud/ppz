// termws drives the web terminal's WebSocket for e2e tests — the browser
// side of terminal control (internal/server/terminal_control.go) without
// a browser. Built into the test-runner image.
//
//	termws -url ws://…/terminal/ws -cookie <ppz_session> [-origin O] STEP...
//
// Steps run in order:
//
//	take | release          send {"type":"take"} / {"type":"release"}
//	send:TEXT               send TEXT followed by "\r" as a binary frame
//	expect:TYPE             wait ≤10s for a text frame of TYPE (control,
//	                        lease, control-error), skipping others, and
//	                        print it on one line
//	sleep:DUR               wait (Go duration)
//
// Printing only what a step asks for keeps output deterministic when
// other clients' lease changes interleave.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	url := flag.String("url", "", "terminal WebSocket URL")
	cookie := flag.String("cookie", "", "ppz_session cookie value")
	origin := flag.String("origin", "", "Origin header (defaults to the URL's http origin)")
	flag.Parse()

	o := *origin
	if o == "" {
		o = "http://" + strings.SplitN(strings.TrimPrefix(*url, "ws://"), "/", 2)[0]
	}
	h := http.Header{}
	h.Set("Cookie", "ppz_session="+*cookie)
	h.Set("Origin", o)

	ctx := context.Background()
	conn, resp, err := websocket.Dial(ctx, *url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		fmt.Printf("dial failed: http=%d\n", code)
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	frames := make(chan map[string]any, 64)
	go func() {
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				close(frames)
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var m map[string]any
			if json.Unmarshal(data, &m) == nil {
				frames <- m
			}
		}
	}()

	for _, step := range flag.Args() {
		name, arg, _ := strings.Cut(step, ":")
		switch name {
		case "take", "release":
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"`+name+`"}`))
		case "send":
			_ = conn.Write(ctx, websocket.MessageBinary, []byte(arg+"\r"))
		case "sleep":
			d, _ := time.ParseDuration(arg)
			time.Sleep(d)
		case "expect":
			if !expect(frames, arg) {
				fmt.Printf("timeout waiting for %s\n", arg)
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown step %q\n", step)
			os.Exit(2)
		}
	}
}

func expect(frames <-chan map[string]any, typ string) bool {
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-frames:
			if !ok {
				return false
			}
			if m["type"] != typ {
				continue
			}
			switch typ {
			case "control":
				fmt.Printf("control writable=%v you=%v\n", m["writable"], m["you"])
			case "lease":
				fmt.Printf("lease holder=%v\n", m["holder"])
			default:
				fmt.Printf("%s %v\n", typ, m["error"])
			}
			return true
		case <-deadline:
			return false
		}
	}
}
