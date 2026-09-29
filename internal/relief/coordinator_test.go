package relief

import (
	"strings"
	"testing"

	"github.com/wyw14/solo-039/internal/model"
)

// 缺阀位回执：即使放散侧已泄压、命令本身没有报错，停机仍不得宣告安全，
// 且结果必须携带不能安全的原因。
func TestCombineShutdownPendingReceiptIsUnsafe(t *testing.T) {
	reliefState := model.Relief{Released: true}
	reason := "steam stop command #2 has no matching valve-position acknowledgement"

	result := CombineShutdown(nil, nil, reliefState, false, true, reason)
	if result.Safe() {
		t.Fatalf("shutdown must not be safe while the valve receipt is pending")
	}
	if !result.Depressurized {
		t.Fatalf("depressurization status should still be reported")
	}
	if result.SteamClosed {
		t.Fatalf("steam must not be reported closed without a matching receipt")
	}
	if !result.SteamPending || result.SteamReason != reason {
		t.Fatalf("pending flag and reason must propagate: %+v", result)
	}
	if err := result.Error(); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("pending state must surface as the shutdown error, got %v", err)
	}
}

// 同代次回执闭合、且无待确认项时，停机才安全。
func TestCombineShutdownConfirmedReceiptIsSafe(t *testing.T) {
	result := CombineShutdown(nil, nil, model.Relief{Released: true}, true, false, "")
	if !result.Safe() {
		t.Fatalf("depressurized + matched closure should be safe: %+v", result)
	}
	if result.Error() != nil {
		t.Fatalf("confirmed shutdown must carry no error, got %v", result.Error())
	}
}

// 有命令错误时按错误处理，不把它误报为单纯待确认。
func TestCombineShutdownSteamErrorTakesPrecedence(t *testing.T) {
	result := CombineShutdown(nil, errSteam, model.Relief{Released: true}, false, true, "should be suppressed")
	if result.SteamPending {
		t.Fatalf("a failed command must not be presented merely as pending")
	}
	if result.Error() != errSteam {
		t.Fatalf("steam error must surface, got %v", result.Error())
	}
	if result.Safe() {
		t.Fatalf("shutdown with a steam command error must be unsafe")
	}
}

var errSteam = steamErr("steam valve did not respond")

type steamErr string

func (e steamErr) Error() string { return string(e) }
