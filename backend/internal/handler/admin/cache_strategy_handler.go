package admin

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type CacheStrategyHandler struct {
	service *service.CacheStrategyService
}

func NewCacheStrategyHandler(svc *service.CacheStrategyService) *CacheStrategyHandler {
	return &CacheStrategyHandler{service: svc}
}

type CacheStrategyRequest struct {
	Name             string                 `json:"name" binding:"required"`
	Description      string                 `json:"description"`
	Enabled          *bool                  `json:"enabled"`
	Config           map[string]interface{} `json:"config"`
	ExpectedRevision int64                  `json:"expected_revision"`
}

type CacheStrategyBindingsRequest struct {
	GroupIDs []int64 `json:"group_ids"`
}

func (h *CacheStrategyHandler) List(c *gin.Context) {
	items, err := h.service.List(c.Request.Context(), strings.TrimSpace(c.Query("search")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	type row struct {
		*service.CacheStrategy
		BoundGroupCount int `json:"bound_group_count"`
	}
	out := make([]row, 0, len(items))
	for i := range items {
		count, err := h.service.BoundGroupCount(c, items[i].ID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		item := items[i]
		out = append(out, row{CacheStrategy: &item, BoundGroupCount: count})
	}
	response.Success(c, out)
}

func (h *CacheStrategyHandler) Get(c *gin.Context) {
	id, ok := parseCacheStrategyID(c)
	if !ok {
		return
	}
	item, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if item == nil {
		response.NotFound(c, "Cache strategy not found")
		return
	}
	count, err := h.service.BoundGroupCount(c, id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"strategy": item, "bound_group_count": count})
}

func (h *CacheStrategyHandler) Create(c *gin.Context) {
	var req CacheStrategyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	cfg, err := configFromRequest(req.Config)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	item, err := h.service.Create(c.Request.Context(), &service.CacheStrategy{
		Name: strings.TrimSpace(req.Name), Description: req.Description, Enabled: enabled, Config: cfg,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *CacheStrategyHandler) Update(c *gin.Context) {
	id, ok := parseCacheStrategyID(c)
	if !ok {
		return
	}
	var req CacheStrategyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	existing, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if existing == nil {
		response.NotFound(c, "Cache strategy not found")
		return
	}
	if req.Config != nil {
		existing.Config, err = configFromRequest(req.Config)
		if err != nil {
			response.BadRequest(c, err.Error())
			return
		}
	}
	if strings.TrimSpace(req.Name) != "" {
		existing.Name = strings.TrimSpace(req.Name)
	}
	existing.Description = req.Description
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.ExpectedRevision > 0 && existing.Revision != req.ExpectedRevision {
		response.Error(c, 409, "Cache strategy revision conflict")
		return
	}
	updated, err := h.service.Update(c.Request.Context(), existing, existing.Revision)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, updated)
}

func (h *CacheStrategyHandler) Duplicate(c *gin.Context) {
	id, ok := parseCacheStrategyID(c)
	if !ok {
		return
	}
	source, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if source == nil {
		response.NotFound(c, "Cache strategy not found")
		return
	}
	name := source.Name + " copy"
	var req struct {
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&req)
	if strings.TrimSpace(req.Name) != "" {
		name = strings.TrimSpace(req.Name)
	}
	copy := &service.CacheStrategy{Name: name, Description: source.Description, Enabled: source.Enabled, Config: source.Config}
	created, err := h.service.Create(c.Request.Context(), copy)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, created)
}

func (h *CacheStrategyHandler) SetEnabled(c *gin.Context, enabled bool) {
	id, ok := parseCacheStrategyID(c)
	if !ok {
		return
	}
	item, err := h.service.SetEnabled(c.Request.Context(), id, enabled)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if item == nil {
		response.NotFound(c, "Cache strategy not found")
		return
	}
	response.Success(c, item)
}

func (h *CacheStrategyHandler) Enable(c *gin.Context)  { h.SetEnabled(c, true) }
func (h *CacheStrategyHandler) Disable(c *gin.Context) { h.SetEnabled(c, false) }

func (h *CacheStrategyHandler) Delete(c *gin.Context) {
	id, ok := parseCacheStrategyID(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func (h *CacheStrategyHandler) Groups(c *gin.Context) {
	id, ok := parseCacheStrategyID(c)
	if !ok {
		return
	}
	groups, err := h.service.ListBoundGroups(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	type groupRow struct {
		ID              int64  `json:"id"`
		Name            string `json:"name"`
		Platform        string `json:"platform"`
		CacheStrategyID *int64 `json:"cache_strategy_id,omitempty"`
	}
	out := make([]groupRow, 0, len(groups))
	for i := range groups {
		out = append(out, groupRow{
			ID:              groups[i].ID,
			Name:            groups[i].Name,
			Platform:        groups[i].Platform,
			CacheStrategyID: groups[i].CacheStrategyID,
		})
	}
	response.Success(c, out)
}

func (h *CacheStrategyHandler) BindGroups(c *gin.Context) {
	id, ok := parseCacheStrategyID(c)
	if !ok {
		return
	}
	var req CacheStrategyBindingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := h.service.BindGroups(c.Request.Context(), id, req.GroupIDs); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"group_ids": req.GroupIDs})
}

// ReplaceGroups is used by the strategy editor to submit the complete
// desired binding set. It preserves groups already owned by this strategy
// while still rejecting groups owned by another strategy.
func (h *CacheStrategyHandler) ReplaceGroups(c *gin.Context) {
	id, ok := parseCacheStrategyID(c)
	if !ok {
		return
	}
	var req CacheStrategyBindingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := h.service.ReplaceGroups(c.Request.Context(), id, req.GroupIDs); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"group_ids": req.GroupIDs})
}

func configFromRequest(raw map[string]interface{}) (service.CacheStrategyConfig, error) {
	if raw == nil {
		return service.DefaultCacheStrategyConfig(service.CacheStrategyKindPrefix), nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return service.CacheStrategyConfig{}, fmt.Errorf("marshal config: %w", err)
	}
	kind := service.CacheStrategyKindPrefix
	if rawKind, ok := raw["kind"].(string); ok && strings.TrimSpace(rawKind) != "" {
		kind = strings.TrimSpace(rawKind)
	}
	cfg := service.DefaultCacheStrategyConfig(kind)
	if err := json.Unmarshal(b, &cfg); err != nil {
		return service.CacheStrategyConfig{}, fmt.Errorf("invalid config: %w", err)
	}
	return service.NormalizeCacheStrategyConfig(cfg)
}

func parseCacheStrategyID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid cache strategy ID")
		return 0, false
	}
	return id, true
}
