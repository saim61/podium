package auth

import "context"

type ctxKey int

const keyUserID ctxKey = iota

// WithUserID stores the authenticated user's id on the context.
func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, keyUserID, userID)
}

// UserIDFrom returns the authenticated user's id, and false when the request is anonymous.
func UserIDFrom(ctx context.Context) (int64, bool) {
	userID, ok := ctx.Value(keyUserID).(int64)
	return userID, ok && userID > 0
}
