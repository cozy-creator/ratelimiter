package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Plan represents a rate limiting plan in the database
type Plan struct {
	bun.BaseModel `bun:"table:ratelimit.plan"`

	ID        string    `bun:"id,pk"`
	Name      string    `bun:"name,notnull"`
	Policies  []byte    `bun:"policies,notnull"` // MessagePack encoded policies
	IsDefault bool      `bun:"is_default"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

// Account represents a user account in the database
type Account struct {
	bun.BaseModel `bun:"table:ratelimit.account"`

	ID        string    `bun:"id,pk"`
	PlanID    string    `bun:"plan_id,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

// AccountPolicies represents the cached policies for an account
type AccountPolicies struct {
	bun.BaseModel `bun:"table:ratelimit.account_policies"`

	AccountID string    `bun:"account_id,pk"`
	Policies  []byte    `bun:"policies,notnull"` // MessagePack encoded policies
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

type QuotaBlock struct {
	bun.BaseModel `bun:"table:ratelimit.quota_block,alias:qb"`

	ID        string    `bun:"id,pk,type:uuid,default:gen_random_uuid()"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp"`
	AccountID string    `bun:"account_id,notnull,type:uuid"`
	Credits   int64     `bun:"credits,notnull"`
	ExpiresAt time.Time `bun:"expires_at"`
	Metadata  []byte    `bun:"metadata,type:jsonb"`
}

// DefaultPlan represents the current default plan
type DefaultPlan struct {
	bun.BaseModel `bun:"table:ratelimit.default_plan"`

	PlanID    string    `bun:"plan_id,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}
