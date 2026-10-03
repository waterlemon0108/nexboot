package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 追平不是拉一次就完：一轮没收全（递归流被拒、改走逐个数据集）就再拉，直到本机完整持有目标那一轮。
func TestCatchUpToPullsUntilTheTargetRoundIsHeld(t *testing.T) {
	held := "rep-100"
	pulls := 0
	err := catchUpTo(context.Background(), "rep-300",
		func(context.Context) error {
			pulls++
			if pulls == 3 {
				held = "rep-300"
			}
			return nil
		},
		func(context.Context) string { return held }, time.Millisecond)
	if err != nil || pulls != 3 {
		t.Fatalf("err=%v pulls=%d", err, pulls)
	}
}

// 拉到比目标还新也算追平：目标取样和拉取之间可能隔着一轮。
func TestCatchUpToAcceptsANewerRound(t *testing.T) {
	err := catchUpTo(context.Background(), "rep-300",
		func(context.Context) error { return nil },
		func(context.Context) string { return "rep-400" }, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
}

// 期限到了还没追平：说清楚停在哪、最后一次为什么失败，接任记录要用。
func TestCatchUpToGivesUpAtTheDeadlineAndSaysWhy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := catchUpTo(ctx, "rep-300",
		func(context.Context) error {
			return errors.New("对端 http://192.168.10.3:8080 返回 500: pool is suspended")
		},
		func(context.Context) string { return "rep-100" }, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "rep-300") || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("err=%v", err)
	}
}
