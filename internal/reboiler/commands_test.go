package reboiler_test

import (
	"context"
	"errors"
	"github.com/wyw14/solo-039/internal/model"
	"github.com/wyw14/solo-039/internal/reboiler"
	"github.com/wyw14/solo-039/internal/relief"
	"testing"
)

func testCampaign(epoch uint64) model.Campaign {
	c := model.NewCampaign("grade-a")
	c.Epoch = epoch
	return c
}

// 正常路径：签发关阀指令后收到匹配回执，命令代次闭合。
func TestCloseConfirmedWithMatchingReceipt(t *testing.T) {
	book := reboiler.NewCommandBook()
	state := reboiler.New()
	campaign := testCampaign(3)

	record, err := book.Close(context.Background(), state, campaign)
	if err != nil {
		t.Fatalf("expected confirmed close, got %v", err)
	}
	if record.Status != reboiler.StatusConfirmed || record.Sequence != 1 {
		t.Fatalf("unexpected record: %+v", record)
	}
	if record.TargetSteam() != 0 {
		t.Fatalf("target steam must be recorded as 0, got %.2f", record.TargetSteam())
	}
	if record.Ack == nil || record.Ack.State != "closed" || record.Ack.ReceivedAt == 0 {
		t.Fatalf("valve receipt missing or incomplete: %+v", record.Ack)
	}
	if record.ConfirmedAt.IsZero() || record.ConfirmedAt.Before(record.IssuedAt) {
		t.Fatal("confirmed time must be recorded after issue time")
	}
	if !book.Confirmed(reboiler.SteamValve, "closed") || !state.ClosureConfirmed(campaign.Epoch) {
		t.Fatal("closure must be confirmed by both the command chain and state")
	}
}

// 逐次记录：每次签发都产生新代次序号，旧代次留档为 stale 且保留其缺失回执的事实。
func TestIssueRecordsEachGeneration(t *testing.T) {
	book := reboiler.NewCommandBook()
	first := testCampaign(1)
	second := testCampaign(2)

	r1, err := book.Issue(first, reboiler.SteamValve, reboiler.ActionClose, 0)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := book.Issue(second, reboiler.SteamValve, reboiler.ActionClose, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Sequence != 1 || r2.Sequence != 2 {
		t.Fatalf("sequences = %d,%d, want 1,2", r1.Sequence, r2.Sequence)
	}

	chain := book.History(reboiler.SteamValve)
	if len(chain) != 2 {
		t.Fatalf("chain length = %d, want 2", len(chain))
	}
	old := chain[0]
	if old.Status != reboiler.StatusStale || old.SupersededBy != 2 {
		t.Fatalf("old generation status=%s supersededBy=%d, want stale/2", old.Status, old.SupersededBy)
	}
	if old.Ack != nil {
		t.Fatal("stale generation must not gain a receipt")
	}
	if old.ClosureSafetyReason() == "" {
		t.Fatal("stale generation must explain why it cannot prove safety")
	}

	active, ok := book.Active(reboiler.SteamValve)
	if !ok || active.Sequence != 2 || !active.Pending() {
		t.Fatalf("active generation must be sequence 2 pending, got %+v ok=%v", active, ok)
	}
}

// 旧代次回执不得替新指令闭合：只回填旧记录并报错，当前代次保持 pending。
func TestStaleReceiptDoesNotCloseNewGeneration(t *testing.T) {
	book := reboiler.NewCommandBook()
	oldCampaign := testCampaign(5)
	newCampaign := testCampaign(6)

	oldCmd, err := book.Issue(oldCampaign, reboiler.SteamValve, reboiler.ActionClose, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book.Issue(newCampaign, reboiler.SteamValve, reboiler.ActionClose, 0); err != nil {
		t.Fatal(err)
	}

	staleAck := model.NewAcknowledgement(oldCmd.Command, "closed", 0, 42)
	err = book.Acknowledge(staleAck)
	if !errors.Is(err, reboiler.ErrStaleAcknowledgement) {
		t.Fatalf("expected ErrStaleAcknowledgement, got %v", err)
	}
	if book.Confirmed(reboiler.SteamValve, "closed") {
		t.Fatal("active generation must not be closed by an old-generation receipt")
	}
	active, _ := book.Active(reboiler.SteamValve)
	if !active.Pending() {
		t.Fatalf("active generation must stay pending, status=%s", active.Status)
	}

	chain := book.History(reboiler.SteamValve)
	if chain[0].Ack == nil || chain[0].Ack.State != "closed" {
		t.Fatal("old-generation receipt must be archived on its own record")
	}
	if chain[0].Status != reboiler.StatusStale {
		t.Fatalf("archived old generation remains stale, got %s", chain[0].Status)
	}
}

// 缺回执：现场阀位反馈缺失时，代次保留待确认，停机不得宣告安全。
func TestCloseWithoutReceiptStaysPending(t *testing.T) {
	book := reboiler.NewCommandBook()
	book.SetValveFeedback(func(model.Command) (string, float64, bool) {
		return "", 0, false
	})
	state := reboiler.New()
	campaign := testCampaign(7)

	record, err := book.Close(context.Background(), state, campaign)
	if !errors.Is(err, reboiler.ErrClosureUnconfirmed) {
		t.Fatalf("expected ErrClosureUnconfirmed, got %v", err)
	}
	if !record.Pending() {
		t.Fatalf("record must remain issued/pending, status=%s", record.Status)
	}
	if record.Ack != nil {
		t.Fatal("no receipt may be fabricated when feedback is missing")
	}
	if reason := record.ClosureSafetyReason(); reason == "" {
		t.Fatal("pending generation must explain why shutdown cannot be declared safe")
	}
	if state.ClosureConfirmed(campaign.Epoch) || book.Confirmed(reboiler.SteamValve, "closed") {
		t.Fatal("closure must not be confirmed without a valve receipt")
	}

	// 停机汇总必须保留待确认状态并带安全说明，Safe() 为假。
	closure := relief.SteamClosure{Pending: true, Generation: record.Sequence, Reason: record.ClosureSafetyReason()}
	result := relief.CombineShutdownWithReceipt(nil, err, model.Relief{Valve: "flare", Released: true}, closure)
	if result.Safe() {
		t.Fatal("shutdown must not be safe while steam closure receipt is missing")
	}
	if !result.SteamPending || result.SteamReason == "" {
		t.Fatalf("result must carry pending state and reason, got %+v", result)
	}
}

// 回执阀位不是关闭位：代次标记为拒动，同样不能闭合。
func TestCloseRejectsWrongValvePosition(t *testing.T) {
	book := reboiler.NewCommandBook()
	book.SetValveFeedback(func(model.Command) (string, float64, bool) {
		return "open", 0.4, true
	})
	state := reboiler.New()
	campaign := testCampaign(8)

	record, err := book.Close(context.Background(), state, campaign)
	if !errors.Is(err, reboiler.ErrClosureUnconfirmed) {
		t.Fatalf("expected ErrClosureUnconfirmed, got %v", err)
	}
	if record.Status != reboiler.StatusRejected {
		t.Fatalf("status = %s, want rejected", record.Status)
	}
	if record.Ack == nil || record.Ack.State != "open" {
		t.Fatalf("wrong-position receipt should be recorded, got %+v", record.Ack)
	}
	if book.Confirmed(reboiler.SteamValve, "closed") {
		t.Fatal("open valve receipt must not confirm closure")
	}
}

// 复盘可按代次逐一核对目标蒸汽量、代次标识、阀位回执与确认时间。
func TestCommandChainSummaryForReview(t *testing.T) {
	book := reboiler.NewCommandBook()
	first := testCampaign(9)
	if _, err := book.Close(context.Background(), reboiler.New(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := book.Issue(testCampaign(10), reboiler.SteamValve, reboiler.ActionClose, 0); err != nil {
		t.Fatal(err)
	}

	summary := book.Summary(reboiler.SteamValve)
	if summary["chain_length"].(int) != 2 {
		t.Fatalf("chain_length=%v, want 2", summary["chain_length"])
	}
	if summary["target_steam"].(float64) != 0 {
		t.Fatalf("target_steam=%v", summary["target_steam"])
	}
	if summary["status"].(string) != reboiler.StatusIssued || !summary["pending"].(bool) {
		t.Fatalf("latest generation must be pending: %+v", summary)
	}
	if summary["safety_reason"].(string) == "" {
		t.Fatal("summary must carry the safety explanation")
	}
	ack := summary["acknowledgement"].(map[string]any)
	if ack["present"].(bool) {
		t.Fatal("pending generation summary must show no receipt")
	}
}
