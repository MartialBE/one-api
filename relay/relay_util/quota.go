package relay_util

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"
	"one-api/types"
	"time"

	"github.com/gin-gonic/gin"
)

type Quota struct {
	modelName        string
	promptTokens     int
	price            model.Price
	groupRatio       float64
	inputRatio       float64
	preConsumedQuota int
	userId           int
	channelId        int
	tokenId          int
	HandelStatus     bool
}

func NewQuota(c *gin.Context, modelName string, promptTokens int) (*Quota, *types.OpenAIErrorWithStatusCode) {
	quota := &Quota{
		modelName:    modelName,
		promptTokens: promptTokens,
		userId:       c.GetInt("id"),
		channelId:    c.GetInt("channel_id"),
		tokenId:      c.GetInt("token_id"),
		HandelStatus: false,
	}

	quota.price = *PricingInstance.GetPrice(quota.modelName)
	quota.groupRatio = common.GetGroupRatio(c.GetString("group"))
	quota.inputRatio = quota.price.GetInput() * quota.groupRatio

	if quota.price.Type == model.TimesPriceType {
		quota.preConsumedQuota = int(1000 * quota.inputRatio)
	} else if quota.price.Input != 0 || quota.price.Output != 0 {
		quota.preConsumedQuota = int(float64(quota.promptTokens)*quota.inputRatio) + config.PreConsumedQuota
	}

	errWithCode := quota.preQuotaConsumption()
	if errWithCode != nil {
		return nil, errWithCode
	}

	return quota, nil
}

func (q *Quota) preQuotaConsumption() *types.OpenAIErrorWithStatusCode {
	if q.preConsumedQuota == 0 {
		return nil
	}

	userQuota, err := model.CacheGetUserQuota(q.userId)
	if err != nil {
		return common.ErrorWrapper(err, "获取用户配额失败", http.StatusInternalServerError)
	}

	if userQuota < q.preConsumedQuota {
		return common.ErrorWrapper(errors.New("用户配额不足"), "用户配额不足", http.StatusForbidden)
	}

	err = model.CacheDecreaseUserQuota(q.userId, q.preConsumedQuota)
	if err != nil {
		return common.ErrorWrapper(err, "减少用户配额失败", http.StatusInternalServerError)
	}

	if userQuota > 100*q.preConsumedQuota {
		// 在这种情况下，我们不预先消耗配额
		// 因为用户有足够的配额
		q.preConsumedQuota = 0
		// common.LogInfo(c.Request.Context(), fmt.Sprintf("user %d has enough quota %d, trusted and no need to pre-consume", userId, userQuota))
	}

	if q.preConsumedQuota > 0 {
		err := model.PreConsumeTokenQuota(q.tokenId, q.preConsumedQuota)
		if err != nil {
			return common.ErrorWrapper(err, "预消耗令牌配额失败", http.StatusForbidden)
		}
		q.HandelStatus = true
	}

	return nil
}

func (q *Quota) completedQuotaConsumption(usage *types.Usage, tokenName string, ctx context.Context) error {
	quota := 0
	promptTokens := usage.PromptTokens
	completionTokens := usage.CompletionTokens

	if q.price.Type == model.TimesPriceType {
		quota = int(1000 * q.inputRatio)
	} else {
		completionRatio := q.price.GetOutput() * q.groupRatio
		quota = int(math.Ceil((float64(promptTokens) * q.inputRatio) + (float64(completionTokens) * completionRatio)))
	}

	if q.inputRatio != 0 && quota <= 0 {
		quota = 1
	}
	totalTokens := promptTokens + completionTokens
	if totalTokens == 0 {
		// 在这种情况下，必须发生了一些错误
		// 我们不能直接返回，因为我们可能需要返还预消耗的配额
		quota = 0
	}
	quotaDelta := quota - q.preConsumedQuota
	err := model.PostConsumeTokenQuota(q.tokenId, quotaDelta)
	if err != nil {
		return errors.New("消耗令牌剩余配额出错: " + err.Error())
	}
	err = model.CacheUpdateUserQuota(q.userId)
	if err != nil {
		return errors.New("消耗令牌剩余配额出错: " + err.Error())
	}

	requestTime := 0
	requestStartTimeValue := ctx.Value("requestStartTime")
	if requestStartTimeValue != nil {
		requestStartTime, ok := requestStartTimeValue.(time.Time)
		if ok {
			requestTime = int(time.Since(requestStartTime).Milliseconds())
		}
	}
	var modelRatioStr string
	if q.price.Type == model.TimesPriceType {
		modelRatioStr = fmt.Sprintf("$%s/次", q.price.FetchInputCurrencyPrice(model.DollarRate))
	} else {
		// 如果输入费率和输出费率一样，则只显示一个费率
		if q.price.GetInput() == q.price.GetOutput() {
			modelRatioStr = fmt.Sprintf("$%s/1k", q.price.FetchInputCurrencyPrice(model.DollarRate))
		} else {
			modelRatioStr = fmt.Sprintf("$%s/1k (输入) | $%s/1k (输出)", q.price.FetchInputCurrencyPrice(model.DollarRate), q.price.FetchOutputCurrencyPrice(model.DollarRate))
		}
	}

	logContent := fmt.Sprintf("模型费率 %s，分组倍率 %.2f", modelRatioStr, q.groupRatio)
	model.RecordConsumeLog(ctx, q.userId, q.channelId, promptTokens, completionTokens, q.modelName, tokenName, quota, logContent, requestTime)
	model.UpdateUserUsedQuotaAndRequestCount(q.userId, quota)
	model.UpdateChannelUsedQuota(q.channelId, quota)

	return nil
}

func (q *Quota) Undo(c *gin.Context) {
	tokenId := c.GetInt("token_id")
	if q.HandelStatus {
		go func(ctx context.Context) {
			// 返还预消耗的配额
			err := model.PostConsumeTokenQuota(tokenId, -q.preConsumedQuota)
			if err != nil {
				logger.LogError(ctx, "返还预消耗配额出错: "+err.Error())
			}
		}(c.Request.Context())
	}
}

func (q *Quota) Consume(c *gin.Context, usage *types.Usage) {
	tokenName := c.GetString("token_name")
	// 如果没有报错，则消费配额
	go func(ctx context.Context) {
		err := q.completedQuotaConsumption(usage, tokenName, ctx)
		if err != nil {
			logger.LogError(ctx, err.Error())
		}
	}(c.Request.Context())
}

func (q *Quota) GetInputRatio() float64 {
	return q.inputRatio
}
