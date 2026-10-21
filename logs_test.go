package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLogBufferRecordsAndForwards(t *testing.T) {
	var out bytes.Buffer
	b := NewLogBuffer(2)
	log := slog.New(b.Handler(slog.NewTextHandler(&out, nil))).With("pr", "ethyca/a#1")

	log.Debug("hidden") // below the inner handler's level: neither kept nor printed
	log.Info("one", "n", 1)
	log.Warn("two", "err", "exit status 1")
	log.Error("three")

	got := b.Recent()
	if len(got) != 2 || got[0].Msg != "two" || got[1].Msg != "three" {
		t.Fatalf("ring should keep the last 2, got %+v", got)
	}
	if got[0].Level != "WARN" || got[0].Attrs != `pr=ethyca/a#1 err="exit status 1"` || got[0].Seq != 2 {
		t.Fatalf("entry = %+v", got[0])
	}
	if !strings.Contains(out.String(), "msg=one") || strings.Contains(out.String(), "hidden") {
		t.Fatalf("inner handler output: %s", out.String())
	}
}

func TestLogBufferSubscribe(t *testing.T) {
	b := NewLogBuffer(10)
	log := slog.New(b.Handler(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	ch, cancel := b.Subscribe()
	log.Info("live")
	select {
	case e := <-ch:
		if e.Msg != "live" {
			t.Fatalf("got %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no entry delivered")
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
	log.Info("after cancel") // must not panic on a closed channel
}
