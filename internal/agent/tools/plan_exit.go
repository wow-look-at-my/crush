package tools

import (
	"context"
	_ "embed"
	"fmt"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/permission"
)

// PlanExitToolName is the tool the agent calls to leave plan mode. It
// is only advertised while plan mode is active; the permission prompt
// it raises is the user's approve/reject decision on the plan.
const PlanExitToolName = "plan_exit"

//go:embed plan_exit.md
var planExitDescription string

// PlanExitParams are the model-facing parameters for the plan_exit tool.
type PlanExitParams struct {
	Plan string `json:"plan" description:"The complete implementation plan to present to the user for approval, formatted as concise markdown"`
}

// PlanExitPermissionsParams carries the plan text into the permission
// prompt so the user reviews exactly what they are approving.
type PlanExitPermissionsParams struct {
	Plan string `json:"plan"`
}

// NewPlanExitTool creates the plan_exit tool. exit is invoked after the
// user approves the plan; it is responsible for switching the
// permission mode back to default and refreshing the agent's tool list
// so the run can continue straight into implementation.
func NewPlanExitTool(permissions permission.Service, workingDir string, exit func(context.Context) error) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		PlanExitToolName,
		planExitDescription,
		func(ctx context.Context, params PlanExitParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Plan == "" {
				return fantasy.NewTextErrorResponse("plan is required: provide the full implementation plan to present to the user"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for exiting plan mode")
			}

			granted, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        workingDir,
					ToolCallID:  call.ID,
					ToolName:    PlanExitToolName,
					Action:      "execute",
					Description: "Approve the plan and exit plan mode",
					Params:      PlanExitPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !granted {
				// Mirror NewPermissionDeniedResponse: stop the turn so
				// the user can give feedback, but tell the model what a
				// rejection means for the next turn.
				resp := fantasy.NewTextErrorResponse("The user rejected the plan. Stay in plan mode and refine the plan based on their feedback before asking for approval again.")
				resp.StopTurn = true
				return resp, nil
			}

			if err := exit(ctx); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to exit plan mode: %w", err)
			}
			return fantasy.NewTextResponse("The user approved the plan. Plan mode is off and the full toolset is available again: implement the plan now."), nil
		},
	)
}
