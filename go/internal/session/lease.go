package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

// 会话 discovery 租约：复刻 acquireSessionDiscoveryLease / renew / release。
//
// 用途：转发前抢占「本会话的 discovery 权」，只在抢到租约的请求上做供应商发现，
// 其余请求复用绑定结果并刷新租约；会话结束后释放。

// LeaseAcquireResult 是获取租约的结果。
type LeaseAcquireResult struct {
	// Acquired 为 true 表示本请求拿到了租约；OwnerToken 非空。
	Acquired   bool
	OwnerToken string
	// Conflict 为 true 表示租约已由他人持有。
	Conflict bool
}

// LeaseMutationResult 是续期/释放的结果。
type LeaseMutationResult struct {
	// OK 为 true 表示操作成功（renewed 或 released）。
	OK bool
	// Lost 为 true 表示 token 不是当前 owner（续期/释放失败）。
	Lost bool
}

// AcquireLease 复刻 acquireSessionDiscoveryLease：SET key ownerToken EX ttl NX。
// ownerToken 为空时生成一个（Node 的 input.ownerToken ?? randomUUID）。
func (b *Binder) AcquireLease(
	ctx context.Context, sessionID string, keyID int64, ownerToken string, ttlSeconds int,
) (LeaseAcquireResult, error) {
	if !b.Ready() {
		return LeaseAcquireResult{}, ErrNilClient
	}
	if keyID <= 0 || ttlSeconds <= 0 || sessionID == "" {
		return LeaseAcquireResult{Conflict: true}, nil
	}
	if ownerToken == "" {
		ownerToken = GenerateToken()
	}
	key := DiscoveryLeaseKey(sessionID, keyID)
	ok, err := b.client.rc.Raw().SetNX(ctx, key, ownerToken, time.Duration(ttlSeconds)*time.Second).Result()
	if err != nil {
		return LeaseAcquireResult{}, err
	}
	if !ok {
		return LeaseAcquireResult{Conflict: true}, nil
	}
	return LeaseAcquireResult{Acquired: true, OwnerToken: ownerToken}, nil
}

// RenewLease 复刻 renewSessionDiscoveryLease。
func (b *Binder) RenewLease(
	ctx context.Context, sessionID string, keyID int64, ownerToken string, ttlSeconds int,
) (LeaseMutationResult, error) {
	if !b.Ready() {
		return LeaseMutationResult{}, ErrNilClient
	}
	if keyID <= 0 || ttlSeconds <= 0 || ownerToken == "" || sessionID == "" {
		return LeaseMutationResult{Lost: true}, nil
	}
	values, err := b.client.evalInt(ctx, "RENEW_SESSION_DISCOVERY_LEASE",
		[]string{DiscoveryLeaseKey(sessionID, keyID)},
		[]any{ownerToken, strconv.Itoa(ttlSeconds)})
	if err != nil {
		return LeaseMutationResult{}, err
	}
	return leaseMutation(values), nil
}

// ReleaseLease 复刻 releaseSessionDiscoveryLease。
func (b *Binder) ReleaseLease(
	ctx context.Context, sessionID string, keyID int64, ownerToken string,
) (LeaseMutationResult, error) {
	if !b.Ready() {
		return LeaseMutationResult{}, ErrNilClient
	}
	if keyID <= 0 || ownerToken == "" || sessionID == "" {
		return LeaseMutationResult{Lost: true}, nil
	}
	values, err := b.client.evalInt(ctx, "RELEASE_SESSION_DISCOVERY_LEASE",
		[]string{DiscoveryLeaseKey(sessionID, keyID)},
		[]any{ownerToken})
	if err != nil {
		return LeaseMutationResult{}, err
	}
	return leaseMutation(values), nil
}

// leaseMutation 把续期/释放脚本的 0/1 回复译成结果。
func leaseMutation(value int64) LeaseMutationResult {
	if value <= 0 {
		return LeaseMutationResult{Lost: true}
	}
	return LeaseMutationResult{OK: true}
}

// GenerateToken 生成租约 owner token（Node 的 randomUUID 等价：32 位十六进制）。
func GenerateToken() string {
	var random [16]byte
	// crypto/rand.Read 在 Linux 上不会失败；万一失败就用全零，不阻断租约获取。
	_, _ = rand.Read(random[:])
	return hex.EncodeToString(random[:])
}
