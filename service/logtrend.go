package service

import (
	"fmt"
	"time"

	"switchdev/db"
)

// TrendPoint 趋势图一个数据点
type TrendPoint = db.TrendPoint

// UsageTrend 使用趋势结果
type UsageTrend = db.UsageTrend

// ComputeUsageTrend 按粒度统计使用趋势（从文件读取，兼容旧数据）
func ComputeUsageTrend(startDate, endDate string, granularity string) *UsageTrend {
	if startDate == "" || endDate == "" {
		endDate = time.Now().Format("2006-01-02")
		startDate = time.Now().AddDate(0, 0, -6).Format("2006-01-02")
	}
	if granularity != "hour" && granularity != "day" {
		granularity = "day"
	}

	logs := getLogsByRange(startDate, endDate, 0)

	var buckets []TrendPoint
	if granularity == "hour" {
		d, _ := time.Parse("2006-01-02", startDate)
		end, _ := time.Parse("2006-01-02", endDate)
		if end.IsZero() {
			end = time.Now()
		}
		for !d.After(end) {
			for h := 0; h < 24; h++ {
				buckets = append(buckets, TrendPoint{
					Label: d.Format("01-02 ") + pad2(h) + ":00",
				})
			}
			d = d.AddDate(0, 0, 1)
		}
	} else {
		d, _ := time.Parse("2006-01-02", startDate)
		end, _ := time.Parse("2006-01-02", endDate)
		if end.IsZero() {
			end = time.Now()
		}
		for !d.After(end) {
			buckets = append(buckets, TrendPoint{Label: d.Format("01-02")})
			d = d.AddDate(0, 0, 1)
		}
	}

	bucketIndex := map[string]int{}
	for i, b := range buckets {
		bucketIndex[b.Label] = i
	}

	for _, log := range logs {
		if log.Status != "success" {
			continue
		}
		var key string
		in := int64(log.InputTokens)
		out := int64(log.OutputTokens)

		if granularity == "hour" {
			if len(log.DateTime) >= 14 {
				key = log.DateTime[5:10] + " " + log.DateTime[11:13] + ":00"
			}
		} else {
			if len(log.DateTime) >= 10 {
				key = log.DateTime[5:10]
			}
		}

		if idx, ok := bucketIndex[key]; ok {
			buckets[idx].Tokens += in + out
			buckets[idx].InputTokens += in
			buckets[idx].OutputTokens += out
			buckets[idx].Reqs++
			if log.CacheHitTokens > 0 {
				buckets[idx].CacheHitTokens += int64(log.CacheHitTokens)
				buckets[idx].CacheHitReqs++
			}
		}
	}

	return &UsageTrend{
		StartDate:   startDate,
		EndDate:     endDate,
		Granularity: granularity,
		Points:      buckets,
	}
}

// GetUsageTrend 暴露给前端的趋势方法（优先从 db 查，回退到文件扫描）
func (s *LogService) GetUsageTrend(startDate, endDate string, granularity string) *UsageTrend {
	if s.core.DB() != nil {
		trend, err := s.core.DB().ComputeUsageTrend(startDate, endDate, granularity)
		if err == nil && trend != nil {
			return trend
		}
	}
	return ComputeUsageTrend(startDate, endDate, granularity)
}

func pad2(n int) string {
	return fmt.Sprintf("%02d", n)
}
