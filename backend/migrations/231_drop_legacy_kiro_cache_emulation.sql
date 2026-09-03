-- Cache shaping is now configured only by groups.cache_strategy_id.
-- Do not retain the retired Kiro-specific group cache emulation controls.

ALTER TABLE groups
  DROP CONSTRAINT IF EXISTS groups_kiro_cache_emulation_ratio_range,
  DROP CONSTRAINT IF EXISTS groups_kiro_cache_emulation_mode_valid,
  DROP CONSTRAINT IF EXISTS groups_kiro_cache_creation_emulation_ratio_range,
  DROP CONSTRAINT IF EXISTS groups_kiro_cache_read_emulation_ratio_range;

ALTER TABLE groups
  DROP COLUMN IF EXISTS kiro_cache_emulation_enabled,
  DROP COLUMN IF EXISTS kiro_cache_emulation_ratio,
  DROP COLUMN IF EXISTS kiro_cache_emulation_mode,
  DROP COLUMN IF EXISTS kiro_cache_creation_emulation_ratio,
  DROP COLUMN IF EXISTS kiro_cache_read_emulation_ratio;
