package main

import (
	"context"
)

type WithOptions struct {
}

func AttemptRequest(ctx context.Context, requestID, userID, endpointID string, tokens int64, opts ...WithOptions) (bool, error) {

	// If credits are consumed, first fetch the credit blocks from postgres, and see if we can
	// consume the desired number of credits.
	//
	// First, fetch the plan for userID from postgres. If none is found, use default plan
	//
	// Then fetch that plan from postgres (a map, encoded in messagepack)
	// Decode the messagepack,,and grab the rate limiter policy for endpointID from postgres.
	// If undefined, that means there is no access to that endpoint, and the request is rejected.
	// 
	// For sliding window, fixed window, and token bucket, fetch the limiter state from redis.
	// If undefined, start off the limiter state as empty.
	//
	// Compare the limiter state to the limits provided, and if any of them are exceeded, reject.
	// Otherwise increment and continue.
	//
	// Finally, do any concurrent-requests checks; if there is a limit like 'only 4 requests live at a time'
	// then check that as well and increment / decrement that.
	//
	// The limiters + concurrency check needs to be done together in an atomic step using SETNX.
	//
	// Return result to caller 
	//
	// Async afterwards (non blocking): 
	// Deduct credits from Postgres.
	// 
	// Log the request to Kafka -> clickhouse, for usage-based billing.
	//
	// some of the stuff from postgres can be cached in memory, since it's redundant to fetch
	// the same plan definition 100 times.
}

func main() {

}