package plugin

import (
	"cmp"
	"net/http"
	"strings"

	"cpa-super-billing/internal/billing"
)

func (a *App) createPlan(req ManagementRequest) ManagementResponse {
	var body struct {
		billing.Plan
		Scopes []string `json:"scopes,omitempty"`
	}
	if errDecode := decodeStrict(req.Body, &body); errDecode != nil {
		return errorResponse(errDecode)
	}
	if refusal, refused := a.ccRefuseSharedPlan(cmp.Or(strings.TrimSpace(body.ID), strings.TrimSpace(body.Name)), body.SharedQuota, body.Scopes); refused {
		return refusal
	}
	stored, errCreate := a.store.CreatePlanWithBindings(body.Plan, body.Scopes)
	if errCreate != nil {
		return errorResponse(errCreate)
	}
	return JSONResponse(http.StatusCreated, map[string]any{"plan": stored})
}

func (a *App) updatePlan(req ManagementRequest) ManagementResponse {
	var body struct {
		billing.PlanPatch
		Scopes *[]string `json:"scopes,omitempty"`
	}
	if errDecode := decodeStrict(req.Body, &body); errDecode != nil {
		return errorResponse(errDecode)
	}
	if refusal, refused := a.ccRefuseSharedPlanUpdate(body.PlanPatch, body.Scopes); refused {
		return refusal
	}
	stored, errUpdate := a.store.UpdatePlanWithBindings(body.PlanPatch, body.Scopes)
	if errUpdate != nil {
		return errorResponse(errUpdate)
	}
	return JSONResponse(http.StatusOK, map[string]any{"plan": stored})
}

func (a *App) resetPlan(req ManagementRequest) ManagementResponse {
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	result, err := a.store.ResetPlanQuota(body.ID)
	if err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, result)
}

func (a *App) deletePlan(req ManagementRequest) ManagementResponse {
	id := strings.TrimSpace(req.Query.Get("id"))
	unbound, errDelete := a.store.DeletePlan(id)
	if errDelete != nil {
		return errorResponse(errDelete)
	}
	return JSONResponse(http.StatusOK, map[string]any{"deleted": id, "unbound_keys": unbound})
}
