package relief

import (
	"context"
	"errors"
	"github.com/wyw14/solo-039/internal/model"
)

type Coordinator struct{ State *State }

type SteamClosure struct {
	// Closed 为当前停热代次已收到匹配的关阀回执。
	Closed bool
	// Pending 为当前代次已签发但阀位回执缺失，仍处待确认。
	Pending bool
	// Generation 为当前停热命令代次序号。
	Generation int
	// TargetSteam 为本代次指令的目标蒸汽量。
	TargetSteam float64
	// Reason 说明蒸汽隔离为何不能视为安全（Pending/拒动/旧代次时非空）。
	Reason string
}

type ShutdownResult struct {
	ReliefError   error
	SteamError    error
	Depressurized bool
	SteamClosed   bool
	// SteamPending 表示停热指令尚待阀位回执闭合，停机不得宣告安全。
	SteamPending bool
	// SteamGeneration 为未闭合停热命令的代次序号。
	SteamGeneration int
	// SteamReason 为缺回执/拒动时的安全说明。
	SteamReason string
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

func CombineShutdown(reliefErr, steamErr error, reliefState model.Relief, steamClosed bool) ShutdownResult {
	return CombineShutdownWithReceipt(reliefErr, steamErr, reliefState, SteamClosure{Closed: steamClosed})
}

// CombineShutdownWithReceipt 汇总泄压与停热两条确认链。缺回执的停热代次会在结果中
// 保留待确认状态与安全说明，使 Safe() 为假并阻止停机被宣告安全。
func CombineShutdownWithReceipt(reliefErr, steamErr error, reliefState model.Relief, steam SteamClosure) ShutdownResult {
	result := ShutdownResult{
		ReliefError:     reliefErr,
		SteamError:      steamErr,
		Depressurized:   reliefErr == nil && reliefState.Released && !reliefState.TimedOut,
		SteamClosed:     steamErr == nil && steam.Closed,
		SteamPending:    steam.Pending,
		SteamGeneration: steam.Generation,
		SteamReason:     steam.Reason,
	}
	if result.SteamPending && result.SteamReason == "" && steamErr != nil {
		result.SteamReason = steamErr.Error()
	}
	return result
}

func (r ShutdownResult) Error() error {
	if r.ReliefError != nil {
		return r.ReliefError
	}
	if r.SteamError != nil {
		return r.SteamError
	}
	if r.SteamPending {
		return errors.New(r.SteamReason)
	}
	return nil
}

func (r ShutdownResult) Safe() bool {
	return r.Depressurized && r.SteamClosed && !r.SteamPending && r.Error() == nil
}
