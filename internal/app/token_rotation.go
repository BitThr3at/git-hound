package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/google/go-github/v57/github"
)

// TokenManager manages GitHub token rotation
type TokenManager struct {
	tokens       []string
	currentIndex int
	clients      []*github.Client
	rateLimits   []time.Time // Track when each token's rate limit resets
	invalidTokens []bool     // Track which tokens are invalid (401 errors)
	mutex        sync.RWMutex
}

var tokenManager *TokenManager
var tokenManagerOnce sync.Once

// GetTokenManager returns the singleton token manager instance
func GetTokenManager() *TokenManager {
	tokenManagerOnce.Do(func() {
		tokenManager = &TokenManager{}
		tokenManager.initialize()
	})
	return tokenManager
}

// initialize sets up the token manager with available tokens
func (tm *TokenManager) initialize() {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	// Collect all available tokens
	flags := GetFlags()
	
	// Add tokens from --tokens flag
	if len(flags.GithubTokens) > 0 {
		tm.tokens = append(tm.tokens, flags.GithubTokens...)
	}
	
	// Add single token for backward compatibility if no tokens were provided via --tokens
	if len(tm.tokens) == 0 && flags.GithubAccessToken != "" {
		tm.tokens = append(tm.tokens, flags.GithubAccessToken)
	}

	if len(tm.tokens) == 0 {
		color.Red("[!] No GitHub tokens available. Please provide tokens via --tokens flag or config file.")
		return
	}

	// Initialize clients and rate limit tracking
	tm.clients = make([]*github.Client, len(tm.tokens))
	tm.rateLimits = make([]time.Time, len(tm.tokens))
	tm.invalidTokens = make([]bool, len(tm.tokens))
	
	for i, token := range tm.tokens {
		tm.clients[i] = github.NewClient(nil).WithAuthToken(token)
		tm.rateLimits[i] = time.Time{} // No rate limit initially
		tm.invalidTokens[i] = false    // Assume valid initially
	}

	if GetFlags().Debug {
		color.Green("[+] Initialized token rotation with %d tokens", len(tm.tokens))
	}
}

// GetClient returns the current GitHub client, rotating if necessary
func (tm *TokenManager) GetClient() *github.Client {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	if len(tm.clients) == 0 {
		return nil
	}

	// Find the first valid, non-rate-limited token
	startIndex := tm.currentIndex
	for {
		// Skip invalid tokens
		if !tm.invalidTokens[tm.currentIndex] && time.Now().After(tm.rateLimits[tm.currentIndex]) {
			return tm.clients[tm.currentIndex]
		}
		
		tm.currentIndex = (tm.currentIndex + 1) % len(tm.tokens)
		if tm.currentIndex == startIndex {
			break // Avoid infinite loop
		}
	}

	// Return current client even if it's not ideal
	return tm.clients[tm.currentIndex]
}

// GetCurrentToken returns the current token
func (tm *TokenManager) GetCurrentToken() string {
	tm.mutex.RLock()
	defer tm.mutex.RUnlock()

	if len(tm.tokens) == 0 {
		return ""
	}

	return tm.tokens[tm.currentIndex]
}

// GetCurrentValidToken returns the current token if it's valid, or rotates to find a valid one
func (tm *TokenManager) GetCurrentValidToken() string {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	if len(tm.tokens) == 0 {
		return ""
	}

	// Find the first valid, non-rate-limited token
	startIndex := tm.currentIndex
	for {
		// Skip invalid tokens
		if !tm.invalidTokens[tm.currentIndex] && time.Now().After(tm.rateLimits[tm.currentIndex]) {
			return tm.tokens[tm.currentIndex]
		}
		
		tm.currentIndex = (tm.currentIndex + 1) % len(tm.tokens)
		if tm.currentIndex == startIndex {
			break // Avoid infinite loop
		}
	}

	// Return current token even if it's not ideal
	return tm.tokens[tm.currentIndex]
}

// RotateToken moves to the next available token
func (tm *TokenManager) RotateToken() *github.Client {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	if len(tm.clients) <= 1 {
		// No other tokens to rotate to
		return tm.clients[tm.currentIndex]
	}

	startIndex := tm.currentIndex
	
	// Find the next token that's valid and not rate limited
	for {
		tm.currentIndex = (tm.currentIndex + 1) % len(tm.tokens)
		
		// Skip invalid tokens
		if tm.invalidTokens[tm.currentIndex] {
			if tm.currentIndex == startIndex {
				break // Avoid infinite loop
			}
			continue
		}
		
		// Check if this token's rate limit has reset
		if time.Now().After(tm.rateLimits[tm.currentIndex]) {
			if GetFlags().Debug {
				color.Cyan("[*] Rotated to token %d/%d", tm.currentIndex+1, len(tm.tokens))
			}
			return tm.clients[tm.currentIndex]
		}
		
		// If we've cycled through all tokens, return to the original one
		if tm.currentIndex == startIndex {
			break
		}
	}

	// All valid tokens are rate limited, return the current one
	if GetFlags().Debug {
		color.Yellow("[!] All valid tokens are rate limited")
	}
	return tm.clients[tm.currentIndex]
}

// SetRateLimit marks the current token as rate limited until the given time
func (tm *TokenManager) SetRateLimit(resetTime time.Time) {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	tm.rateLimits[tm.currentIndex] = resetTime
	
	if GetFlags().Debug {
		color.Yellow("[!] Token %d/%d rate limited until %s", tm.currentIndex+1, len(tm.tokens), resetTime.Format("15:04:05"))
	}
}

// MarkTokenInvalid marks the current token as invalid (for 401 errors)
func (tm *TokenManager) MarkTokenInvalid() {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	tm.invalidTokens[tm.currentIndex] = true
	
	if GetFlags().Debug {
		color.Red("[!] Token %d/%d marked as invalid (401 Bad credentials)", tm.currentIndex+1, len(tm.tokens))
	}
}

// GetTokenCount returns the number of available tokens
func (tm *TokenManager) GetTokenCount() int {
	tm.mutex.RLock()
	defer tm.mutex.RUnlock()
	
	return len(tm.tokens)
}

// ValidateTokens tests all tokens to ensure they're valid
func (tm *TokenManager) ValidateTokens() error {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	if len(tm.clients) == 0 {
		return fmt.Errorf("no tokens available")
	}

	validTokens := 0
	for i, client := range tm.clients {
		_, _, err := client.Users.Get(context.Background(), "")
		if err != nil {
			tm.invalidTokens[i] = true
			if GetFlags().Debug {
				color.Yellow("[!] Token %d/%d is invalid: %v", i+1, len(tm.tokens), err)
			}
		} else {
			tm.invalidTokens[i] = false
			validTokens++
			if GetFlags().Debug {
				color.Green("[+] Token %d/%d is valid", i+1, len(tm.tokens))
			}
		}
	}

	if validTokens == 0 {
		return fmt.Errorf("no valid tokens found")
	}

	if GetFlags().Debug {
		color.Green("[+] %d/%d tokens are valid", validTokens, len(tm.tokens))
	}

	return nil
}

// GetAvailableTokensCount returns the number of tokens that are valid and not rate limited
func (tm *TokenManager) GetAvailableTokensCount() int {
	tm.mutex.RLock()
	defer tm.mutex.RUnlock()

	available := 0
	now := time.Now()
	
	for i, resetTime := range tm.rateLimits {
		if !tm.invalidTokens[i] && now.After(resetTime) {
			available++
		}
	}
	
	return available
} 