package reboiler

import (
	"context"
	"errors"
	"fmt"
	"github.com/wyw14/solo-039/internal/model"
	"sync"
	"time"
)

const (
	SteamValve  = "steam-valve"
	ActionClose = "close"

	// StatusIssued 命令已签发，正在等待对应代次的阀位回执。
	StatusIssued = "issued"
	// StatusConfirmed 已收到与本代次严格匹配的阀位回执，且阀位确为关闭位。
	StatusConfirmed = "confirmed"
	// StatusRejected 回执属于本代次，但阀位不在关闭位（阀卡、拒动）。
	StatusRejected = "rejected"
	// StatusStale 该代次已被更新的指令取代，未在其生命周期内闭合。
	StatusStale = "stale"
)

var (
	// ErrClosureUnconfirmed 停热指令缺少匹配的关阀回执，蒸汽隔离未经证实。
	ErrClosureUnconfirmed = errors.New("steam closure unconfirmed")
	// ErrStaleAcknowledgement 回执属于已被取代的旧代次，不能替当前指令闭合。
	ErrStaleAcknowledgement = errors.New("acknowledgement belongs to a superseded command generation")
	// ErrUnknownAcknowledgement 回执不对应任何已签发的命令代次。
	ErrUnknownAcknowledgement = errors.New("acknowledgement does not correspond to any issued command")
)

// CommandRecord 是停热命令链上的一环：一次指令与其回执、时间戳终生配对留档，
// 事故复盘时可按序号逐次核对“目标蒸汽量 → 命令代次 → 阀位回执 → 确认时间”。
type CommandRecord struct {
	// Sequence 是该设备命令链上的逐次代次序号（从 1 起）。
	Sequence int
	// Command 携带命令 ID、campaign/revision/epoch 三代标识与目标蒸汽量（Setpoint）。
	Command model.Command
	// IssuedAt 为指令签发时间。
	IssuedAt time.Time
	// Ack 为现场返回的阀位回执；nil 表示仍处待确认状态。
	Ack *model.Acknowledgement
	// ConfirmedAt 为收到匹配回执、闭合本代次的时间；零值表示未闭合。
	ConfirmedAt time.Time
	// Status 见 Status* 常量。
	Status string
	// Note 记录取代、拒动、回执缺失等复盘说明。
	Note string
	// SupersededBy 为取代本代次的新代次序号（0 表示仍为当前代次）。
	SupersededBy int
}

// TargetSteam 返回本次指令要求的目标蒸汽量。
func (r CommandRecord) TargetSteam() float64 { return r.Command.Setpoint }

// Generation 返回本代次的完整代次标识，供复盘配对。
func (r CommandRecord) Generation() string {
	return fmt.Sprintf("seq=%d epoch=%d revision=%s command=%s", r.Sequence, r.Command.Epoch, r.Command.Revision, r.Command.ID)
}

// Pending 表示本代次仍缺阀位回执。
func (r CommandRecord) Pending() bool { return r.Status == StatusIssued }

// ClosureSafetyReason 说明本代次为何不能作为“停机安全”的依据；已确认时返回空串。
func (r CommandRecord) ClosureSafetyReason() string {
	switch r.Status {
	case StatusConfirmed:
		return ""
	case StatusRejected:
		return fmt.Sprintf("steam closure generation %d rejected: %s; valve is not proven at closed position, steam supply may still reach the reboiler", r.Sequence, r.Note)
	case StatusStale:
		return fmt.Sprintf("steam closure generation %d was superseded by generation %d without a matching receipt: %s; current generation is unproven, residual heat and pressure cannot be ruled out", r.Sequence, r.SupersededBy, r.Note)
	default:
		return fmt.Sprintf("steam closure generation %d (epoch %d, target steam %.2f) issued at %s has no valve-position acknowledgement; steam isolation is unverified, so the shutdown must remain pending and cannot be declared safe", r.Sequence, r.Command.Epoch, r.Command.Setpoint, r.IssuedAt.Format(time.RFC3339))
	}
}

// ValveFeedback 模拟/接入现场阀位反馈：返回实际阀位、读数与是否成功送达。
// ok=false 表示回执缺失（仪表故障、链路丢失），命令必须保留待确认。
type ValveFeedback func(command model.Command) (state string, value float64, ok bool)

// CommandBook 按设备保存逐次命令链。active 只记录每个设备当前代次的序号，
// 历史代次全部留档，旧代次的回执只回填旧记录，绝不替新指令闭合。
type CommandBook struct {
	mu       sync.Mutex
	history  map[string][]CommandRecord
	active   map[string]int
	feedback ValveFeedback
}

func NewCommandBook() *CommandBook {
	return &CommandBook{
		history:  make(map[string][]CommandRecord),
		active:   make(map[string]int),
		feedback: SimulatedValveFeedback,
	}
}

// SetValveFeedback 替换现场阀位反馈来源（例如注入“回执缺失”故障）。
func (b *CommandBook) SetValveFeedback(f ValveFeedback) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if f == nil {
		f = SimulatedValveFeedback
	}
	b.feedback = f
}

// SimulatedValveFeedback 是默认的模拟现场反馈：关阀指令回到 closed/0。
func SimulatedValveFeedback(c model.Command) (string, float64, bool) {
	if c.Action == ActionClose {
		return "closed", 0, true
	}
	return c.Action, c.Setpoint, true
}

// Issue 签发新一代次指令；同设备上未闭合的旧代次转为 stale 留档，不得再被闭合。
func (b *CommandBook) Issue(campaign model.Campaign, equipment, action string, setpoint float64) (CommandRecord, error) {
	command := model.NewCommand(campaign, equipment, action, setpoint)
	if err := command.Validate(); err != nil {
		return CommandRecord{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sequence := len(b.history[equipment]) + 1
	if prior := b.active[equipment]; prior > 0 {
		record := b.history[equipment][prior-1]
		record.Status = StatusStale
		record.SupersededBy = sequence
		record.Note = fmt.Sprintf("superseded at %s by generation %d before a matching receipt arrived", time.Now().Format(time.RFC3339), sequence)
		b.history[equipment][prior-1] = record
	}
	record := CommandRecord{Sequence: sequence, Command: command, IssuedAt: time.Now(), Status: StatusIssued}
	b.history[equipment] = append(b.history[equipment], record)
	b.active[equipment] = sequence
	return record, nil
}

// Acknowledge 接收阀位回执。回执必须与某一代次严格配对：
// 旧代次回执只回填旧记录并返回 ErrStaleAcknowledgement，绝不闭合当前指令；
// 不属于任何代次的回执返回 ErrUnknownAcknowledgement。
func (b *CommandBook) Acknowledge(value model.Acknowledgement) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	idx := b.locateLocked(value)
	if idx.index < 0 {
		if prior := b.findSupersededLocked(value); prior != nil {
			b.attachLocked(*prior, value)
			return fmt.Errorf("%w: receipt %s matches generation %d, not the active generation %d", ErrStaleAcknowledgement, value.State, b.history[prior.equipment][prior.index].Sequence, b.active[prior.equipment])
		}
		return ErrUnknownAcknowledgement
	}
	record := b.history[idx.equipment][idx.index]
	command := record.Command
	if err := value.Validate(command); err != nil {
		return err
	}
	b.attachLocked(idx, value)
	if value.State == "closed" && value.Value == 0 {
		record = b.history[idx.equipment][idx.index]
		record.Status = StatusConfirmed
		record.ConfirmedAt = time.Unix(0, value.ReceivedAt)
		b.history[idx.equipment][idx.index] = record
		return nil
	}
	record = b.history[idx.equipment][idx.index]
	record.Status = StatusRejected
	record.Note = fmt.Sprintf("valve reported position %q at %.3f on %s, expected closed/0", value.State, value.Value, time.Unix(0, value.ReceivedAt).Format(time.RFC3339))
	b.history[idx.equipment][idx.index] = record
	return nil
}

type historyIndex struct {
	equipment string
	index     int
}

// locateLocked 返回与回执严格匹配的当前代次位置；不匹配返回 -1。
func (b *CommandBook) locateLocked(value model.Acknowledgement) historyIndex {
	active, ok := b.active[value.Equipment]
	if !ok {
		return historyIndex{index: -1}
	}
	record := b.history[value.Equipment][active-1]
	if value.Matches(record.Command) {
		return historyIndex{equipment: value.Equipment, index: active - 1}
	}
	return historyIndex{index: -1}
}

// findSupersededLocked 在旧代次中寻找回执的归属，仅用于回填留档。
func (b *CommandBook) findSupersededLocked(value model.Acknowledgement) *historyIndex {
	for i := len(b.history[value.Equipment]) - 1; i >= 0; i-- {
		record := b.history[value.Equipment][i]
		if value.Matches(record.Command) {
			if b.active[value.Equipment] == record.Sequence {
				return nil
			}
			return &historyIndex{equipment: value.Equipment, index: i}
		}
	}
	return nil
}

func (b *CommandBook) attachLocked(idx historyIndex, value model.Acknowledgement) {
	record := b.history[idx.equipment][idx.index]
	if record.Ack == nil {
		ack := value
		record.Ack = &ack
		b.history[idx.equipment][idx.index] = record
	}
}

// Confirmed 报告当前代次是否已被指定阀位状态的匹配回执闭合。
func (b *CommandBook) Confirmed(equipment, state string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	sequence := b.active[equipment]
	if sequence == 0 {
		return false
	}
	record := b.history[equipment][sequence-1]
	return record.Status == StatusConfirmed && record.Ack != nil && record.Ack.State == state
}

// Pending 返回设备当前代次是否仍缺阀位回执。
func (b *CommandBook) Pending(equipment string) (CommandRecord, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sequence := b.active[equipment]
	if sequence == 0 {
		return CommandRecord{}, false
	}
	record := b.history[equipment][sequence-1]
	return record, record.Status == StatusIssued
}

// Active 返回设备的当前代次记录。
func (b *CommandBook) Active(equipment string) (CommandRecord, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sequence := b.active[equipment]
	if sequence == 0 {
		return CommandRecord{}, false
	}
	return b.history[equipment][sequence-1], true
}

// Latest 返回设备最近一次签发的代次记录（含已被取代的旧代次）。
func (b *CommandBook) Latest(equipment string) (CommandRecord, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	chain := b.history[equipment]
	if len(chain) == 0 {
		return CommandRecord{}, false
	}
	return chain[len(chain)-1], true
}

// History 返回设备逐次命令链的完整留档副本，供事故复盘。
func (b *CommandBook) History(equipment string) []CommandRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	chain := b.history[equipment]
	out := make([]CommandRecord, len(chain))
	copy(out, chain)
	return out
}

// Summary 返回命令链摘要，供运行状态与操作审计使用。
func (b *CommandBook) Summary(equipment string) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	chain := b.history[equipment]
	if len(chain) == 0 {
		return map[string]any{"chain_length": 0, "pending": false}
	}
	record := chain[len(chain)-1]
	ack := map[string]any{"present": false}
	if record.Ack != nil {
		ack = map[string]any{"present": true, "state": record.Ack.State, "value": record.Ack.Value, "received_at": record.Ack.ReceivedAt, "command": record.Ack.Command}
	}
	confirmedAt := int64(0)
	if !record.ConfirmedAt.IsZero() {
		confirmedAt = record.ConfirmedAt.UnixNano()
	}
	return map[string]any{
		"chain_length":    len(chain),
		"sequence":        record.Sequence,
		"epoch":           record.Command.Epoch,
		"revision":        record.Command.Revision,
		"command":         record.Command.ID,
		"target_steam":    record.Command.Setpoint,
		"status":          record.Status,
		"issued_at":       record.IssuedAt.UnixNano(),
		"acknowledgement": ack,
		"confirmed_at":    confirmedAt,
		"pending":         record.Status == StatusIssued,
		"safety_reason":   record.ClosureSafetyReason(),
		"note":            record.Note,
	}
}

// Close 签发新一代次的关阀停热指令并驱动阀门，随后只接受现场阀位反馈来闭合。
// 反馈缺失或阀位不在关闭位时，代次保留待确认/拒动状态并返回 ErrClosureUnconfirmed，
// 调用方不得据此宣告停机安全。
func (b *CommandBook) Close(ctx context.Context, state *State, campaign model.Campaign) (CommandRecord, error) {
	record, err := b.Issue(campaign, SteamValve, ActionClose, 0)
	if err != nil {
		return CommandRecord{}, err
	}
	if err := state.Close(ctx, record.Command.Epoch); err != nil {
		return record, err
	}
	if err := b.consumeFeedback(state, record); err != nil {
		return record, err
	}
	latest, _ := b.Active(SteamValve)
	if latest.Status != StatusConfirmed {
		return latest, fmt.Errorf("%w: %s", ErrClosureUnconfirmed, latest.ClosureSafetyReason())
	}
	return latest, nil
}

// consumeFeedback 取得现场阀位反馈并按代次入账；缺失时不做任何代替性确认。
func (b *CommandBook) consumeFeedback(state *State, record CommandRecord) error {
	b.mu.Lock()
	feedback := b.feedback
	b.mu.Unlock()
	position, value, ok := feedback(record.Command)
	if !ok {
		b.annotate(record.Sequence, "valve-position receipt missing; generation held in pending state")
		return nil
	}
	ack := model.NewAcknowledgement(record.Command, position, value, time.Now().UnixNano())
	if err := b.Acknowledge(ack); err != nil {
		if errors.Is(err, ErrStaleAcknowledgement) || errors.Is(err, ErrUnknownAcknowledgement) {
			b.annotate(record.Sequence, "receipt rejected: "+err.Error())
			return nil
		}
		return err
	}
	latest, _ := b.Active(SteamValve)
	if latest.Status == StatusConfirmed {
		if !state.AcknowledgeClosed(record.Command.Epoch) {
			b.annotate(record.Sequence, "state rejected acknowledgement generation")
		}
	}
	return nil
}

func (b *CommandBook) annotate(sequence int, note string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	chain := b.history[SteamValve]
	if sequence < 1 || sequence > len(chain) {
		return
	}
	record := chain[sequence-1]
	record.Note = note
	chain[sequence-1] = record
}
