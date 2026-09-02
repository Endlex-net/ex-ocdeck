package notify

import (
	"context"
	"testing"

	"ocdeck/internal/domain/notification"
)

var _ notification.Channel = (*WebChannel)(nil)

// webPublisherFake WebPublisher 窄端口 fake：记录转发的意图，按配置返回 accepted。
type webPublisherFake struct {
	published []notification.Intent
	accepted  bool
}

func (f *webPublisherFake) Publish(in notification.Intent) bool {
	f.published = append(f.published, in)
	return f.accepted
}

func TestWebChannel_ForwardAndAccepted(t *testing.T) {
	fake := &webPublisherFake{accepted: true}
	ch := NewWebChannel(fake)
	in := notification.Intent{
		TaskID:   "task-42",
		TaskName: "demo-task",
		Category: notification.CategoryQuestion,
		Level:    notification.LevelTimeSensitive,
		Title:    "等待你的回答",
		Body:     "demo-task\n这是问题内容",
		URL:      "http://127.0.0.1:18080/#/task/task-42",
	}
	res := ch.Send(context.Background(), in, notification.ChannelConfig{})
	if !res.OK || res.Err != "" {
		t.Fatalf("accepted publish must succeed, got OK=%v Err=%q", res.OK, res.Err)
	}
	if len(fake.published) != 1 || fake.published[0] != in {
		t.Fatalf("publisher must receive the intent unchanged, got %+v", fake.published)
	}
}

// TestWebChannel_RejectedFails accepted=false（零连接或全部缓冲满）计为该渠道
// 投递失败。
func TestWebChannel_RejectedFails(t *testing.T) {
	fake := &webPublisherFake{accepted: false}
	ch := NewWebChannel(fake)
	res := ch.Send(context.Background(), notification.Intent{TaskID: "task-42"}, notification.ChannelConfig{})
	if res.OK {
		t.Fatal("rejected publish must fail")
	}
	if res.Err == "" {
		t.Fatal("failure must carry a reason")
	}
	if len(fake.published) != 1 {
		t.Fatalf("intent must still be forwarded exactly once, got %d", len(fake.published))
	}
}

func TestWebChannel_NameAndCaps(t *testing.T) {
	ch := NewWebChannel(&webPublisherFake{})
	if ch.Name() != "web" {
		t.Fatalf("name = %q, want web", ch.Name())
	}
	if ch.Caps() != notification.CapReplace {
		t.Fatalf("caps = %v, want CapReplace", ch.Caps())
	}
}
