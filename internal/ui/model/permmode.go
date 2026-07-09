package model

import "github.com/charmbracelet/crush/internal/permission"

// permissionModeUI groups the TUI presentation for one permission mode.
// The behavior itself lives in the permission package's policy table;
// this table only holds display strings. Adding a mode means adding an
// entry here (and prompt styles if it should recolor the editor prompt).
type permissionModeUI struct {
	// label names the mode in status messages.
	label string
	// placeholder replaces the editor placeholder while the mode is
	// active (empty keeps the normal placeholder).
	placeholder string
}

var permissionModeUIs = map[permission.Mode]permissionModeUI{
	permission.ModeDefault: {
		label: "default",
	},
	permission.ModeAcceptEdits: {
		label:       "accept edits (file edits in the working directory are auto-approved)",
		placeholder: "Accept-edits mode",
	},
	permission.ModePlan: {
		label:       "plan (read-only; tools update on the next message)",
		placeholder: "Plan mode",
	},
}

// permissionModeUIFor returns the presentation entry for the mode,
// falling back to the default entry for unknown modes.
func permissionModeUIFor(mode permission.Mode) permissionModeUI {
	if ui, ok := permissionModeUIs[mode]; ok {
		return ui
	}
	return permissionModeUIs[permission.ModeDefault]
}

// nextPermissionMode returns the mode after cur in the cycle order.
func nextPermissionMode(cur permission.Mode) permission.Mode {
	modes := permission.Modes()
	for i, mode := range modes {
		if mode == cur {
			return modes[(i+1)%len(modes)]
		}
	}
	return permission.ModeDefault
}
