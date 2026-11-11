package cursor

import "strings"

// TrialResult 单账号 $250 Sand Trial 激活结果
type TrialResult struct {
	ID     int    `json:"id"`
	Email  string `json:"email,omitempty"`
	Status string `json:"status"` // activated / already / not_eligible / failed
	Err    string `json:"err,omitempty"`
}

// OK 激活是否视为成功(已激活/订阅覆盖/本次激活均算成功)
func (r TrialResult) OK() bool { return r.Status != "failed" }

// ⚠️ 已移除 ActivateTrial / ActivateAllTrials：二者签名依赖 ai2api 的 *Store。
// Sand Trial 激活在 sub2api 侧属管理端动作，由 service 层调用 Client.StartSandTrial
// 后自行写库，协议层不持有账号集合。

// Sand Trial 激活结果状态。
const (
	TrialStatusActivated   = "activated"
	TrialStatusAlready     = "already"
	TrialStatusNotEligible = "not_eligible"
	TrialStatusFailed      = "failed"
)

// ClassifyStartSandTrialError 把 StartSandTrial 的结果归类成激活状态。
// 幂等：已激活过（already funded / already_exists / already been used / already granted）
// 与订阅已覆盖（not eligible）都视为成功，只有其余错误才算失败。
func ClassifyStartSandTrialError(err error) string {
	if err == nil {
		return TrialStatusActivated
	}
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "already funded"),
		strings.Contains(low, "already_exists"),
		strings.Contains(low, "already been used"),
		strings.Contains(low, "already granted"):
		return TrialStatusAlready
	case strings.Contains(low, "not eligible"):
		return TrialStatusNotEligible
	default:
		return TrialStatusFailed
	}
}
