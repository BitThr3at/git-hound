package app

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/google/go-github/v57/github"
)

// Semaphore to limit concurrent HTTP requests
var httpSemaphore chan struct{}
var httpSemaphoreOnce sync.Once

func initHTTPSemaphore() {
	httpSemaphoreOnce.Do(func() {
		threads := GetFlags().Threads
		if threads <= 0 {
			threads = 10
		}
		httpSemaphore = make(chan struct{}, threads)
	})
}

func SearchWithAPI(queries []string) {
	// Initialize HTTP request limiter
	initHTTPSemaphore()

	// Initialize token manager
	tokenManager := GetTokenManager()
	if tokenManager.GetTokenCount() == 0 {
		color.Red("[!] No GitHub tokens available. Please provide tokens via --tokens flag, config file, or GITHOUND_GITHUB_TOKEN environment variable.")
		os.Exit(1)
	}

	// Validate all tokens
	if err := tokenManager.ValidateTokens(); err != nil {
		color.Red("[!] %v", err)
		os.Exit(1)
	}

	client := tokenManager.GetClient()
	if client == nil {
		color.Red("[!] Unable to create GitHub client. Please check your configuration.")
		os.Exit(1)
	}

	if !GetFlags().ResultsOnly && !GetFlags().JsonOutput && GetFlags().Debug {
		color.Cyan("[*] Logged into GitHub using token rotation (%d tokens available)", tokenManager.GetTokenCount())
	}

	options := github.SearchOptions{
		Sort: "indexed",
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}

	// Create a simple HTTP client for downloading raw files (without GitHub API authentication)
	createHTTPClient := func() *http.Client {
		client := &http.Client{}
		rt := WithHeader(client.Transport)
		rt.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_13_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.132 Safari/537.36")
		client.Transport = rt
		return client
	}

	for _, query := range queries {
		if GetFlags().Dashboard && GetFlags().InsertKey != "" {
			BrokerSearchCreation(query)
		}
		for page := 0; page < int(math.Min(10, float64(GetFlags().Pages))); page++ {
			options.Page = page
			if GetFlags().Debug {
				TrackAPIRequest("Search.Code", fmt.Sprintf("Query: %s, Page: %d", query, page))
			}
			
			// Get current client (may have been rotated)
			client = tokenManager.GetClient()
			result, _, err := client.Search.Code(context.Background(), query, &options)
			
			for err != nil {
				fmt.Println(err)
				resetTime := extractResetTime(err.Error())
				
				// Check for invalid token (401 error)
				if isInvalidTokenError(err) {
					tokenManager.MarkTokenInvalid()
					
					// Try to rotate to another valid token
					client = tokenManager.RotateToken()
					
					// Check if we have any valid tokens left
					availableTokens := tokenManager.GetAvailableTokensCount()
					if availableTokens == 0 {
						color.Red("[!] All tokens are invalid. Please check your GitHub tokens.")
						os.Exit(1)
					} else {
						color.Cyan("[*] Rotated to next valid token (%d tokens still available)", availableTokens)
					}
				} else if resetTime > 0 {
					// Mark current token as rate limited
					tokenManager.SetRateLimit(time.Now().Add(time.Duration(resetTime+3) * time.Second))
					
					// Try to rotate to another token
					client = tokenManager.RotateToken()
					
					// Check if we have any available tokens
					availableTokens := tokenManager.GetAvailableTokensCount()
					if availableTokens == 0 {
						sleepDuration := resetTime + 3
						color.Yellow("[!] All valid GitHub tokens are rate limited. Waiting %d seconds...", sleepDuration)
						time.Sleep(time.Duration(sleepDuration) * time.Second)
					} else {
						color.Cyan("[*] Rotated to next available token (%d tokens still available)", availableTokens)
					}
				} else {
					// Non-rate-limit error, wait a bit and retry
					time.Sleep(5 * time.Second)
				}
				
				if GetFlags().Debug {
					TrackAPIRequest("Search.Code", fmt.Sprintf("Query: %s, Page: %d (retry)", query, page))
				}
				result, _, err = client.Search.Code(context.Background(), query, &options)
			}

			// If we get an empty page of results, stop searching
			if len(result.CodeResults) == 0 {
				if GetFlags().Debug {
					fmt.Println("No more results found, stopping search...")
				}
				break
			}

			if !GetFlags().ResultsOnly && !GetFlags().JsonOutput && GetFlags().Debug {
				fmt.Println("Analyzing " + strconv.Itoa(len(result.CodeResults)) + " repos on page " + strconv.Itoa(page+1) + "...")
			}

			// Initialize the worker pool if not already done
			workerPool := GetGlobalPool()

			for _, code_result := range result.CodeResults {
				// fmt.Println(code_result.GetPath())
				author_repo_str := code_result.GetRepository().GetOwner().GetLogin() + "/" + code_result.GetRepository().GetName()
				re := regexp.MustCompile(`\/([a-f0-9]{40})\/`)
				matches := re.FindStringSubmatch(code_result.GetHTMLURL())

				sha := ""
				if len(matches) > 1 {
					sha = matches[1]
				}

				// Create a repo result object to pass to the worker
				repoResult := RepoSearchResult{
					Repo:   author_repo_str,
					File:   code_result.GetPath(),
					Raw:    author_repo_str + "/" + sha + "/" + code_result.GetPath(),
					Source: "repo",
					Query:  query,
					URL:    "https://github.com/" + author_repo_str + "/blob/" + sha + "/" + code_result.GetPath(),
				}

				// Increment the wait group before submitting the job
				SearchWaitGroup.Add(1)

				// Submit the job to the worker pool instead of creating a goroutine directly
				workerPool.Submit(func() {
					// Process the repository in the worker pool
					ScanAndPrintResult(createHTTPClient(), repoResult)
				})
			}
		}

		if !GetFlags().ResultsOnly && !GetFlags().JsonOutput {
			color.Green("Finished searching... Now waiting for scanning to finish.")
		}

		SearchWaitGroup.Wait()
		if !GetFlags().ResultsOnly && !GetFlags().JsonOutput {
			color.Green("Finished scanning.")
		}
	}
}

// extractResetTime extracts the number of seconds until the rate limit resets from the error message.
func extractResetTime(errorMessage string) int {
	re := regexp.MustCompile(`rate reset in (\d+)s`)
	matches := re.FindStringSubmatch(errorMessage)
	if len(matches) > 1 {
		seconds, err := strconv.Atoi(matches[1])
		if err == nil {
			return seconds
		}
	}
	return 0
}

// isInvalidTokenError checks if the error indicates an invalid token (401)
func isInvalidTokenError(err error) bool {
	if err == nil {
		return false
	}
	errorMsg := err.Error()
	return strings.Contains(errorMsg, "401") || strings.Contains(errorMsg, "Bad credentials")
}
