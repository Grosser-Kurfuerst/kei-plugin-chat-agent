package chatagent

import (
	"context"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func TestSmokeGroupReplyAndCommands(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"private_policy":            "open",
		"mention_reply_probability": 1.0,
	})
	env.chat.replies = []string{"你好"}
	ev := atEvent("e1", "u1", "hi", "g1", "bot")
	if err := env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply()); err != nil {
		t.Fatalf("handler: %v", err)
	}
	env.timers.fireLast()
	sends := env.bot.waitSends(1, 2*time.Second)
	if len(sends) != 1 {
		t.Fatalf("sends=%d", len(sends))
	}
	if got := messageText(sends[0].msg); got != "你好" {
		t.Fatalf("reply=%q", got)
	}
	if sends[0].target.Kind != bot.MessageGroup || sends[0].target.ChannelID != "g1" || sends[0].target.UserID != "" {
		t.Fatalf("target=%+v", sends[0].target)
	}
	out := runCommand(t, env.p, commandEvent("chat", "status", "admin", "g1"))
	if out == "" {
		t.Fatal("empty status")
	}
}

func TestSmokeStopIdempotent(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	ctx := context.Background()
	if err := env.p.Stop(ctx); err != nil {
		t.Fatalf("stop1: %v", err)
	}
	if err := env.p.Stop(ctx); err != nil {
		t.Fatalf("stop2: %v", err)
	}
}
