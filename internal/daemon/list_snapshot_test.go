package daemon

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/natsubj"
)

func TestStreamInfoByNameListsStreamsOnce(t *testing.T) {
	accountID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	streamName := natsubj.StreamName(accountID, "agent", "broadcast")
	provider := &fakeStreamInfoProvider{
		infos: []*jetstream.StreamInfo{
			{Config: jetstream.StreamConfig{Name: streamName}},
		},
	}

	got, err := streamInfoByName(context.Background(), provider, accountID)
	if err != nil {
		t.Fatalf("streamInfoByName: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("ListStreams calls = %d, want 1", provider.calls)
	}
	if got[streamName] == nil {
		t.Fatalf("streamInfoByName missing %q", streamName)
	}
}

func TestPipesForSourceDedupeSortedAutoAndUserPipes(t *testing.T) {
	got := pipesForSource(cliproto.Source{
		Kind:  string(cliproto.KindPTY),
		Pipes: []string{"archive", "stdout", "alerts"},
	})
	want := []string{"alerts", "archive", "heartbeat", "inbox", "stdctrl", "stdin", "stdout", "system"}
	if len(got) != len(want) {
		t.Fatalf("pipesForSource len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pipesForSource = %v, want %v", got, want)
		}
	}
}

type fakeStreamInfoProvider struct {
	calls int
	infos []*jetstream.StreamInfo
	err   error
}

func (p *fakeStreamInfoProvider) ListStreams(context.Context, ...jetstream.StreamListOpt) jetstream.StreamInfoLister {
	p.calls++
	ch := make(chan *jetstream.StreamInfo, len(p.infos))
	for _, info := range p.infos {
		ch <- info
	}
	close(ch)
	return fakeStreamInfoLister{infos: ch, err: p.err}
}

type fakeStreamInfoLister struct {
	infos <-chan *jetstream.StreamInfo
	err   error
}

func (l fakeStreamInfoLister) Info() <-chan *jetstream.StreamInfo { return l.infos }
func (l fakeStreamInfoLister) Err() error                         { return l.err }
