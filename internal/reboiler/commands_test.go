package reboiler

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wyw14/solo-039/internal/model"
)

func testCampaign(epoch uint64) model.Campaign {
	return model.Campaign{ID: uuid.New(), Revision: uuid.New(), Epoch: epoch, Phase: model.Shutting}
}

func ackFor(command model.Command, state string) model.Acknowledgement {
	return model.Acknowledgement{
		Command:   command.ID,
		Campaign:  command.Campaign,
		Revision:  command.Revision,
		Epoch:     command.Epoch,
		Equipment: command.Equipment,
		State:     state,
		Value:     command.Setpoint,
	}
}

// 逐次记录：两代停热指令都留在链上，目标蒸汽量、代次、回执、确认时间可逐次核对。
func TestCommandBookRecordsEachStopGeneration(t *testing.T) {
	book := NewCommandBook()
	first, err := book.Issue(testCampaign(1), steamValveEquipment, "close", 0)
	if err != nil {
		t.Fatalf("issue first: %v", err)
	}
	second, err := book.Issue(testCampaign(2), steamValveEquipment, "close", 0)
	if err != nil {
		t.Fatalf("issue second: %v", err)
	}

	history := book.History(steamValveEquipment)
	if len(history) != 2 {
		t.Fatalf("expected 2 generations on chain, got %d", len(history))
	}
	if history[0].Sequence != 1 || history[1].Sequence != 2 {
		t.Fatalf("chain sequence broken: %+v", history)
	}
	if history[0].Command.ID != first.ID || history[1].Command.ID != second.ID {
		t.Fatalf("chain does not preserve both command generations")
	}
	if history[0].TargetSteam != 0 || history[1].TargetSteam != 0 {
		t.Fatalf("target steam not recorded per command")
	}
	if history[0].Command.Epoch != 1 || history[1].Command.Epoch != 2 {
		t.Fatalf("command epochs not preserved per command")
	}
	if history[0].IssuedAt.IsZero() || history[1].IssuedAt.IsZero() {
		t.Fatalf("issue time not recorded")
	}
}

// 旧代次回执不得替新指令闭合：先收到第一代的迟到回执，只闭合第一代；
// 最新一代仍处于待确认。
func TestStaleAcknowledgementCannotCloseNewGeneration(t *testing.T) {
	book := NewCommandBook()
	first, _ := book.Issue(testCampaign(1), steamValveEquipment, "close", 0)
	second, _ := book.Issue(testCampaign(2), steamValveEquipment, "close", 0)

	if err := book.Acknowledge(ackFor(first, "closed")); err != nil {
		t.Fatalf("late acknowledgement for first generation rejected: %v", err)
	}

	history := book.History(steamValveEquipment)
	if !history[0].Acknowledged || history[0].ConfirmedAt.IsZero() {
		t.Fatalf("first generation should be closed by its own receipt")
	}
	if history[1].Acknowledged {
		t.Fatalf("old-generation receipt must not close the new command")
	}
	if book.Confirmed(steamValveEquipment, "closed") {
		t.Fatalf("latest generation must not read as confirmed from a stale receipt")
	}

	pending := book.Pending(steamValveEquipment)
	if len(pending) != 1 || pending[0].CommandID != second.ID.String() {
		t.Fatalf("second generation must remain pending, got %+v", pending)
	}

	// 伪造代次（不属于任何已发指令）的回执直接拒绝。
	forged := ackFor(second, "closed")
	forged.Command = uuid.New()
	if err := book.Acknowledge(forged); err == nil {
		t.Fatalf("forged-generation acknowledgement must be rejected")
	}

	// 同代次正确回执到达后才闭合。
	if err := book.Acknowledge(ackFor(second, "closed")); err != nil {
		t.Fatalf("matching acknowledgement rejected: %v", err)
	}
	if !book.ClosureConfirmed(steamValveEquipment) {
		t.Fatalf("latest generation should be confirmed after matching receipt")
	}
	if len(book.Pending(steamValveEquipment)) != 0 {
		t.Fatalf("no generation should remain pending after matching receipt")
	}
}

// 缺回执：命令发出但回执未到，必须保留待确认状态并给出不能宣告安全的原因。
func TestMissingAcknowledgementStaysPendingWithReason(t *testing.T) {
	book := NewCommandBook()
	command, _ := book.Issue(testCampaign(3), steamValveEquipment, "close", 0)

	pending := book.Pending(steamValveEquipment)
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending generation, got %d", len(pending))
	}
	if pending[0].CommandID != command.ID.String() || pending[0].Epoch != 3 {
		t.Fatalf("pending receipt does not identify the unacknowledged command: %+v", pending)
	}
	if book.ClosureConfirmed(steamValveEquipment) {
		t.Fatalf("closure must not be confirmed without a receipt")
	}

	reason := book.PendingReason(steamValveEquipment)
	for _, want := range []string{command.ID.String(), "no matching valve-position acknowledgement", "cannot be declared safe"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("pending reason %q missing %q", reason, want)
		}
	}
}

// 完整关阀链路：同代次回执到位后命令链与设备状态双双闭合，停机可以宣告安全。
func TestCloseHappyPathConfirmsCurrentGeneration(t *testing.T) {
	book := NewCommandBook()
	state := New()
	campaign := testCampaign(5)

	if err := book.Close(context.Background(), state, campaign); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !book.ClosureConfirmed(steamValveEquipment) {
		t.Fatalf("command book should confirm closure")
	}
	if !state.ClosureConfirmed(campaign.Epoch) {
		t.Fatalf("reboiler state should confirm closure for epoch %d", campaign.Epoch)
	}
	history := book.History(steamValveEquipment)
	if len(history) != 1 || history[0].AckState != "closed" || history[0].ConfirmedAt.IsZero() {
		t.Fatalf("closure record incomplete: %+v", history)
	}
}

// 重复回执不能二次闭合同一代次。
func TestDuplicateAcknowledgementRejected(t *testing.T) {
	book := NewCommandBook()
	command, _ := book.Issue(testCampaign(1), steamValveEquipment, "close", 0)
	if err := book.Acknowledge(ackFor(command, "closed")); err != nil {
		t.Fatalf("first acknowledgement: %v", err)
	}
	if err := book.Acknowledge(ackFor(command, "closed")); err == nil {
		t.Fatalf("duplicate acknowledgement must be rejected")
	}
}
