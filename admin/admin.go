package admin

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cozy-creator/ratelimiter/limiters"
	"github.com/cozy-creator/ratelimiter/models"
	"github.com/uptrace/bun"
	"github.com/vmihailenco/msgpack/v5"
	"gopkg.in/yaml.v3"
)

// Plan represents a rate limiting plan configuration
type Plan struct {
	Name      string                        `yaml:"name"`
	IsDefault bool                          `yaml:"is_default"`
	Endpoints map[string]limiters.Policy    `yaml:"endpoints"`
}

// Plans is a map of plan ID to plan configuration
type Plans map[string]Plan

// YAMLConfig represents the structure of the YAML configuration file
type YAMLConfig struct {
	Plans Plans `yaml:"plans"`
}

// LoadPlansFromFile loads plans from a YAML file
func LoadPlansFromFile(path string) (Plans, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	var config YAMLConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}

	return config.Plans, nil
}

// ApplyPlans applies the plan configurations to the database
func ApplyPlans(ctx context.Context, db *bun.DB, plans Plans) error {
	// Start transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Clear existing default plan if we're adding a new one
	for _, plan := range plans {
		if plan.IsDefault {
			_, err = tx.NewUpdate().
				Model((*models.Plan)(nil)).
				Set("is_default = ?", false).
				Where("is_default = ?", true).
				Exec(ctx)
			if err != nil {
				return fmt.Errorf("clearing default plan: %w", err)
			}
			break
		}
	}

	// Insert or update plans
	for id, plan := range plans {
		policies, err := msgpack.Marshal(plan.Endpoints)
		if err != nil {
			return fmt.Errorf("encoding policies: %w", err)
		}

		_, err = tx.NewInsert().
			Model(&models.Plan{
				ID:        id,
				Name:      plan.Name,
				Policies:  policies,
				IsDefault: plan.IsDefault,
			}).
			On("CONFLICT (id) DO UPDATE").
			Set("name = EXCLUDED.name").
			Set("policies = EXCLUDED.policies").
			Set("is_default = EXCLUDED.is_default").
			Set("updated_at = ?", time.Now()).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("upserting plan: %w", err)
		}
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// AssignPlan assigns a plan to an account
func AssignPlan(ctx context.Context, db *bun.DB, accountID, planID string) error {
	_, err := db.NewUpdate().
		Model((*models.Account)(nil)).
		Set("plan_id = ?", planID).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", accountID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("updating account: %w", err)
	}
	return nil
}

// GetAccountPlan gets the current plan for an account
func GetAccountPlan(ctx context.Context, db *bun.DB, accountID string) (*Plan, error) {
	var policies []byte
	err := db.NewSelect().
		Table("ratelimit.account_policies").
		Column("policies").
		Where("account_id = ?", accountID).
		Scan(ctx, &policies)
	if err != nil {
		return nil, fmt.Errorf("finding policies: %w", err)
	}

	var endpoints map[string]limiters.Policy
	err = msgpack.Unmarshal(policies, &endpoints)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling policies: %w", err)
	}

	// Get plan details
	var plan models.Plan
	err = db.NewSelect().
		Model(&plan).
		Join("JOIN ratelimit.account a ON a.plan_id = plan.id").
		Where("a.id = ?", accountID).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding plan: %w", err)
	}

	return &Plan{
		Name:      plan.Name,
		IsDefault: plan.IsDefault,
		Endpoints: endpoints,
	}, nil
}

// ListPlans lists all available plans
func ListPlans(ctx context.Context, db *bun.DB) (Plans, error) {
	var dbPlans []models.Plan
	err := db.NewSelect().
		Model(&dbPlans).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing plans: %w", err)
	}

	plans := make(Plans)
	for _, dbPlan := range dbPlans {
		var endpoints map[string]limiters.Policy
		err = msgpack.Unmarshal(dbPlan.Policies, &endpoints)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling policies: %w", err)
		}

		plans[dbPlan.ID] = Plan{
			Name:      dbPlan.Name,
			IsDefault: dbPlan.IsDefault,
			Endpoints: endpoints,
		}
	}

	return plans, nil
}

// DeletePlan deletes a plan if it's not in use
func DeletePlan(ctx context.Context, db *bun.DB, planID string) error {
	// Check if plan is in use
	exists, err := db.NewSelect().
		Model((*models.Account)(nil)).
		Where("plan_id = ?", planID).
		Exists(ctx)
	if err != nil {
		return fmt.Errorf("checking plan usage: %w", err)
	}
	if exists {
		return fmt.Errorf("plan is in use by one or more accounts")
	}

	// Delete plan
	_, err = db.NewDelete().
		Model((*models.Plan)(nil)).
		Where("id = ?", planID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting plan: %w", err)
	}

	return nil
}

// SetDefaultPlan sets a plan as the default
func SetDefaultPlan(ctx context.Context, db *bun.DB, planID string) error {
	// Start transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Verify plan exists
	exists, err := tx.NewSelect().
		Model((*models.Plan)(nil)).
		Where("id = ?", planID).
		Exists(ctx)
	if err != nil {
		return fmt.Errorf("checking plan existence: %w", err)
	}
	if !exists {
		return fmt.Errorf("plan %s does not exist", planID)
	}

	// Delete existing default plan (if any)
	_, err = tx.NewDelete().
		Model((*models.DefaultPlan)(nil)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("clearing default plan: %w", err)
	}

	// Set new default plan
	_, err = tx.NewInsert().
		Model(&models.DefaultPlan{
			PlanID: planID,
		}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("setting default plan: %w", err)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// GetDefaultPlan gets the current default plan
func GetDefaultPlan(ctx context.Context, db *bun.DB) (*Plan, error) {
	var defaultPlan models.DefaultPlan
	err := db.NewSelect().
		Model(&defaultPlan).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding default plan: %w", err)
	}

	return GetPlan(ctx, db, defaultPlan.PlanID)
}

// GetPlan gets a plan by ID
func GetPlan(ctx context.Context, db *bun.DB, planID string) (*Plan, error) {
	var dbPlan models.Plan
	err := db.NewSelect().
		Model(&dbPlan).
		Where("id = ?", planID).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding plan: %w", err)
	}

	var endpoints map[string]limiters.Policy
	err = msgpack.Unmarshal(dbPlan.Policies, &endpoints)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling policies: %w", err)
	}

	return &Plan{
		Name:      dbPlan.Name,
		Endpoints: endpoints,
	}, nil
} 