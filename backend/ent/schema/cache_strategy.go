package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
)

// CacheStrategy stores the reusable cache shaping policy shared by groups.
// The effective policy is persisted as JSON so new protocol-neutral controls
// can be added without coupling them to the group schema.
type CacheStrategy struct {
	ent.Schema
}

func (CacheStrategy) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").MaxLen(100).NotEmpty().Unique(),
		field.String("description").Default(""),
		field.Bool("enabled").Default(true),
		field.Int64("revision").Default(1),
		field.JSON("config", map[string]any{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}
