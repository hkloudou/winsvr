package update

import (
	"encoding/json"
	"os"
)

func (u *Updater) loadState() State {
	var s State
	data, err := os.ReadFile(u.statePath())
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s)
	return s
}

func (u *Updater) saveState(s State) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(u.statePath(), data, 0o644)
}
