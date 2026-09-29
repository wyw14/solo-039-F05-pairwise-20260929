package relief

import (
	"context"
	"errors"
	"github.com/wyw14/solo-039/internal/model"
)

type Coordinator struct{ State *State }

type ShutdownResult struct {
	ReliefError   error
	SteamError    error
	Depressurized bool
	SteamClosed   bool
	// SteamPending 表示停热指令已发出但缺少同代次阀位回执；SteamReason
	// 用一句话说明为什么这种待确认状态下不能宣告停机安全。
	SteamPending bool
	SteamReason  string
}

func NewCoordinator() *Coordinator { return &Coordinator{State: New()} }

func (c *Coordinator) Shutdown(ctx context.Context, cpg model.Campaign, valve string, timeout bool) error {
	c.State.Begin(valve)
	if timeout {
		c.State.Timeout()
		return errors.New("relief valve timeout")
	}
	c.State.Release()
	return nil
}

func (c *Coordinator) Safe() bool { return c.State.Snapshot().Released && !c.State.Snapshot().TimedOut }

func CombineShutdown(reliefErr, steamErr error, reliefState model.Relief, steamClosed bool, steamPending bool, steamPendingReason string) ShutdownResult {
	pending := steamErr == nil && steamPending
	return ShutdownResult{
		ReliefError:   reliefErr,
		SteamError:    steamErr,
		Depressurized: reliefErr == nil && reliefState.Released && !reliefState.TimedOut,
		SteamClosed:   steamErr == nil && steamClosed && !pending,
		SteamPending:  pending,
		SteamReason:   steamPendingReason,
	}
}

func (r ShutdownResult) Error() error {
	if r.ReliefError != nil {
		return r.ReliefError
	}
	if r.SteamError != nil {
		return r.SteamError
	}
	// 没有命令错误、但回执仍缺失时，待确认本身就是停机不能放行的原因。
	if r.SteamPending {
		return errors.New(r.SteamReason)
	}
	return nil
}

func (r ShutdownResult) Safe() bool {
	return r.Depressurized && r.SteamClosed && !r.SteamPending && r.Error() == nil
}
