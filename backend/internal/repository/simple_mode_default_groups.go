package repository

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const simpleModeDefaultGroupDescription = "Auto-created default group"

func ensureSimpleModeDefaultGroups(ctx context.Context, client *dbent.Client) error {
	if client == nil {
		return fmt.Errorf("nil ent client")
	}

	if err := backfillSimpleModeGrokDefaultImageGeneration(ctx, client); err != nil {
		return err
	}

	// ⚠️ 每接入一个新平台都必须在这里补一行。
	// 漏掉不会报错：SIMPLE 模式下新建该平台账号时，admin_account.go 会按
	// "<platform>-default" 找默认分组，找不到就静默不绑任何分组——账号建好了
	// 却永远调度不到，且界面上看不出任何异常。
	requiredByPlatform := map[string]int{
		service.PlatformAnthropic:   1,
		service.PlatformOpenAI:      1,
		service.PlatformGemini:      1,
		service.PlatformAntigravity: 2,
		service.PlatformKiro:        1,
		service.PlatformCursor:      1,
		service.PlatformGrok:        1,
	}

	for platform, minCount := range requiredByPlatform {
		count, err := client.Group.Query().
			Where(group.PlatformEQ(platform), group.DeletedAtIsNil()).
			Count(ctx)
		if err != nil {
			return fmt.Errorf("count groups for platform %s: %w", platform, err)
		}

		if platform == service.PlatformAntigravity {
			if count < minCount {
				for i := count; i < minCount; i++ {
					name := fmt.Sprintf("%s-default-%d", platform, i+1)
					if err := createGroupIfNotExists(ctx, client, name, platform); err != nil {
						return err
					}
				}
			}
			continue
		}

		// Non-antigravity platforms: ensure <platform>-default exists.
		name := platform + "-default"
		if err := createGroupIfNotExists(ctx, client, name, platform); err != nil {
			return err
		}
	}

	return nil
}

func createGroupIfNotExists(ctx context.Context, client *dbent.Client, name, platform string) error {
	exists, err := client.Group.Query().
		Where(group.NameEQ(name), group.DeletedAtIsNil()).
		Exist(ctx)
	if err != nil {
		return fmt.Errorf("check group exists %s: %w", name, err)
	}
	if exists {
		return nil
	}

	_, err = client.Group.Create().
		SetName(name).
		SetDescription(simpleModeDefaultGroupDescription).
		SetPlatform(platform).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1.0).
		SetIsExclusive(false).
		SetAllowImageGeneration(platform == service.PlatformGrok).
		Save(ctx)
	if err != nil {
		if dbent.IsConstraintError(err) {
			// Concurrent server startups may race on creation; treat as success.
			return nil
		}
		return fmt.Errorf("create default group %s: %w", name, err)
	}
	return nil
}

func backfillSimpleModeGrokDefaultImageGeneration(ctx context.Context, client *dbent.Client) error {
	_, err := client.Group.Update().
		Where(
			group.NameEQ(service.PlatformGrok+"-default"),
			group.PlatformEQ(service.PlatformGrok),
			group.DescriptionEQ(simpleModeDefaultGroupDescription),
			group.StatusEQ(service.StatusActive),
			group.AllowImageGenerationEQ(false),
			group.DeletedAtIsNil(),
		).
		SetAllowImageGeneration(true).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("backfill auto-created grok default image generation: %w", err)
	}
	return nil
}
