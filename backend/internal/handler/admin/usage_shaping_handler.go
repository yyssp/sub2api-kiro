package admin

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *UsageHandler) ShapingStats(c *gin.Context) {
	dimension := c.DefaultQuery("dimension", "account")
	if dimension != "account" && dimension != "group" {
		response.BadRequest(c, "Invalid dimension, use account or group")
		return
	}
	tz := c.DefaultQuery("timezone", "Asia/Shanghai")
	if _, err := time.LoadLocation(tz); err != nil {
		response.BadRequest(c, "Invalid timezone")
		return
	}
	now := timezone.NowInUserLocation(tz)
	start := timezone.StartOfDayInUserLocation(now, tz).AddDate(0, 0, -6)
	end := timezone.StartOfDayInUserLocation(now, tz).AddDate(0, 0, 1)
	startDate, endDate := c.Query("start_date"), c.Query("end_date")
	if startDate != "" || endDate != "" {
		var err error
		start, err = timezone.ParseInUserLocation("2006-01-02", startDate, tz)
		if err != nil {
			response.BadRequest(c, "Invalid start_date format, use YYYY-MM-DD")
			return
		}
		end, err = timezone.ParseInUserLocation("2006-01-02", endDate, tz)
		if err != nil {
			response.BadRequest(c, "Invalid end_date format, use YYYY-MM-DD")
			return
		}
		end = end.AddDate(0, 0, 1)
	}
	if !start.Before(end) {
		response.BadRequest(c, "start_date must not be after end_date")
		return
	}
	if end.After(start.AddDate(0, 0, 366)) {
		response.BadRequest(c, "Date range must not exceed 366 days")
		return
	}
	granularity := c.Query("granularity")
	if granularity == "" {
		granularity = "day"
		if end.Sub(start) <= 49*time.Hour {
			granularity = "hour"
		}
	}
	if granularity != "day" && granularity != "hour" {
		response.BadRequest(c, "Invalid granularity")
		return
	}
	if granularity == "hour" && end.After(start.AddDate(0, 0, 31)) {
		response.BadRequest(c, "Hourly range must not exceed 31 days")
		return
	}
	stats, err := h.usageService.GetShapingStats(c.Request.Context(), service.UsageShapingQuery{Start: start, End: end, Dimension: dimension, Granularity: granularity, Timezone: tz})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, stats)
}
