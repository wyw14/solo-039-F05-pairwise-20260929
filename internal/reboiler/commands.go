package reboiler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wyw14/solo-039/internal/model"
)

// HeatCommandRecord 是停热命令链上的一环：一次停热指令及其阀位回执的完整配对。
// 目标蒸汽量（命令 Setpoint）与命令代次（Command.ID/Revision/Epoch）在发令时落档；
// 阀位回执与确认时间在回执到达时补登。每条记录独立闭合，新指令不会覆盖旧记录。
type HeatCommandRecord struct {
	Sequence     int
	Command      model.Command
	TargetSteam  float64
	IssuedAt     time.Time
	Acknowledged bool
	AckValue     float64
	AckState     string
	ConfirmedAt  time.Time
}

// PendingReceipt 描述一条发出去、却没有拿到同代次阀位回执的停热指令。
type PendingReceipt struct {
	Sequence    int
	CommandID   string
	Epoch       uint64
	TargetSteam float64
	IssuedAt    time.Time
}

const steamValveEquipment = "steam-valve"

type CommandBook struct {
	mu sync.Mutex
	// chain 按设备保存逐次停热命令，最早的在前；新指令追加而不是覆盖。
	chains map[string][]HeatCommandRecord
}

func NewCommandBook() *CommandBook {
	return &CommandBook{chains: make(map[string][]HeatCommandRecord)}
}

// Issue 发出新的一代停热指令并在链尾追加一条记录。旧代次记录原样保留，
// 事故复盘时可以逐次对照指令与回执。
func (b *CommandBook) Issue(campaign model.Campaign, equipment, action string, setpoint float64) (model.Command, error) {
	command := model.NewCommand(campaign, equipment, action, setpoint)
	if err := command.Validate(); err != nil {
		return model.Command{}, err
	}
	b.mu.Lock()
	b.chains[equipment] = append(b.chains[equipment], HeatCommandRecord{
		Sequence:    len(b.chains[equipment]) + 1,
		Command:     command,
		TargetSteam: setpoint,
		IssuedAt:    time.Now(),
	})
	b.mu.Unlock()
	return command, nil
}

// Acknowledge 登记阀位回执。回执必须凭命令代次（Command ID/Revision/Epoch）
// 找回它真正对应的那一条指令：属于旧代次的回执只能挂回旧记录，
// 绝不会替当前最新一代指令闭合。
func (b *CommandBook) Acknowledge(value model.Acknowledgement) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	chain := b.chains[value.Equipment]
	for i := range chain {
		record := &chain[i]
		if value.Command != record.Command.ID {
			continue
		}
		if err := value.Validate(record.Command); err != nil {
			return err
		}
		if record.Acknowledged {
			return fmt.Errorf("command generation %s already acknowledged at %s", record.Command.ID, record.ConfirmedAt.Format(time.RFC3339Nano))
		}
		record.Acknowledged = true
		record.AckState = value.State
		record.AckValue = value.Value
		record.ConfirmedAt = time.Now()
		return nil
	}
	return errors.New("acknowledgement belongs to no issued command generation")
}

// Confirmed 报告该设备最新一代指令是否已由同代次阀位回执闭合。
// 旧代次的确认状态不参与判定：旧回执闭合的是旧记录，新指令仍视为未确认。
func (b *CommandBook) Confirmed(equipment, state string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.latest(equipment)
	if !ok {
		return false
	}
	return record.Acknowledged && record.AckState == state
}

// ClosureConfirmed 与 state 的代次校验保持同一语义：只有最新一代关阀指令
// 拿到状态为 closed 的同代次回执，才算关阀确认。
func (b *CommandBook) ClosureConfirmed(equipment string) bool {
	return b.Confirmed(equipment, "closed")
}

// Pending 返回该设备所有已发出但未收到同代次阀位回执的指令，按发出顺序排列。
// 缺回执的记录一直保留在待确认状态，不会被后续指令或旧回执冲掉。
func (b *CommandBook) Pending(equipment string) []PendingReceipt {
	b.mu.Lock()
	defer b.mu.Unlock()
	var pending []PendingReceipt
	for _, record := range b.chains[equipment] {
		if record.Acknowledged {
			continue
		}
		pending = append(pending, PendingReceipt{
			Sequence:    record.Sequence,
			CommandID:   record.Command.ID.String(),
			Epoch:       record.Command.Epoch,
			TargetSteam: record.TargetSteam,
			IssuedAt:    record.IssuedAt,
		})
	}
	return pending
}

// PendingReason 用一句话说明为什么在缺回执的情况下不能宣告停机安全。
func (b *CommandBook) PendingReason(equipment string) string {
	pending := b.Pending(equipment)
	if len(pending) == 0 {
		return ""
	}
	last := pending[len(pending)-1]
	return fmt.Sprintf(
		"steam stop command #%d (generation %s, epoch %d, target steam %.3f) issued at %s has no matching valve-position acknowledgement: the steam valve cannot be proven closed, so the shutdown must remain unconfirmed and cannot be declared safe",
		last.Sequence, last.CommandID, last.Epoch, last.TargetSteam, last.IssuedAt.Format(time.RFC3339Nano),
	)
}

// History 返回某设备逐次停热命令链的副本，供事故复盘逐次核对
// 目标蒸汽量、命令代次、阀位回执和确认时间。
func (b *CommandBook) History(equipment string) []HeatCommandRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	chain := b.chains[equipment]
	out := make([]HeatCommandRecord, len(chain))
	copy(out, chain)
	return out
}

func (b *CommandBook) latest(equipment string) (HeatCommandRecord, bool) {
	chain := b.chains[equipment]
	if len(chain) == 0 {
		return HeatCommandRecord{}, false
	}
	return chain[len(chain)-1], true
}

// Audit 给出命令链的可序列化视图，供运维审计/接口直接展示。
func (b *CommandBook) Audit(equipment string) map[string]any {
	history := b.History(equipment)
	entries := make([]map[string]any, 0, len(history))
	for _, record := range history {
		entry := map[string]any{
			"sequence":     record.Sequence,
			"command_id":   record.Command.ID.String(),
			"revision":     record.Command.Revision.String(),
			"epoch":        record.Command.Epoch,
			"action":       record.Command.Action,
			"target_steam": record.TargetSteam,
			"issued_at":    record.IssuedAt.UnixNano(),
			"acknowledged": record.Acknowledged,
			"ack_state":    record.AckState,
			"ack_value":    record.AckValue,
			"confirmed_at": record.ConfirmedAt.UnixNano(),
		}
		entries = append(entries, entry)
	}
	pending := b.Pending(equipment)
	pendingIDs := make([]string, 0, len(pending))
	for _, p := range pending {
		pendingIDs = append(pendingIDs, p.CommandID)
	}
	return map[string]any{
		"equipment":      equipment,
		"generations":    entries,
		"latest_closed":  b.Confirmed(equipment, "closed"),
		"pending":        pendingIDs,
		"pending_count":  len(pending),
		"pending_reason": b.PendingReason(equipment),
	}
}

// Close 发出一代新的关阀（停热、目标蒸汽量 0）指令，并等待同代次阀位回执闭合。
// 若回执缺失或属于旧代次，状态停在待确认：返回错误说明为什么不能宣告安全，
// 命令记录仍然保留在链上等待迟到的正确回执。
func (b *CommandBook) Close(ctx context.Context, state *State, campaign model.Campaign) error {
	command, err := b.Issue(campaign, steamValveEquipment, "close", 0)
	if err != nil {
		return err
	}
	if err := state.Close(ctx, command.Epoch); err != nil {
		return err
	}
	acknowledgement := model.Acknowledgement{Command: command.ID, Campaign: command.Campaign, Revision: command.Revision, Epoch: command.Epoch, Equipment: command.Equipment, State: "closed", Value: command.Setpoint}
	if err := b.Acknowledge(acknowledgement); err != nil {
		return err
	}
	if !state.AcknowledgeClosed(command.Epoch) {
		return errors.New("steam closure generation rejected")
	}
	return nil
}
