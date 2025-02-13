/*
 * Copyright (c) 2020 Devtron Labs
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package casbin

import (
	"fmt"
	"github.com/casbin/casbin"
	"github.com/devtron-labs/authenticator/jwt"
	"github.com/devtron-labs/authenticator/middleware"
	"github.com/patrickmn/go-cache"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Enforcer interface {
	Enforce(rvals ...interface{}) bool
	EnforceErr(rvals ...interface{}) error
	EnforceByEmail(rvals ...interface{}) bool
	EnforceByEmailInBatch(emailId string, resource string, action string, vals []string) map[string]bool
	InvalidateCache(emailId string) bool
	InvalidateCompleteCache()
}

func NewEnforcerImpl(
	enforcer *casbin.Enforcer,
	sessionManager *middleware.SessionManager,
	logger *zap.SugaredLogger) *EnforcerImpl {
	lock := make(map[string]*sync.Mutex)
	enf := &EnforcerImpl{lock: lock, Cache: checkCacheEnabled(logger), Enforcer: enforcer, logger: logger, SessionManager: sessionManager}
	setEnforcerImpl(enf)
	return enf
}

func checkCacheEnabled(logger *zap.SugaredLogger) *cache.Cache {
	enableEnforcerCache := os.Getenv("ENFORCER_CACHE")
	enableEnforcerCacheVal, err := strconv.ParseBool(enableEnforcerCache)
	if err != nil {
		logger.Errorw("Error occurred while parsing cache_enable flag", "enableEnforcerCache", enableEnforcerCache, "reason", err)
		enableEnforcerCacheVal = false
	}
	if enableEnforcerCacheVal {
		enforcerCacheExpirationInSec := os.Getenv("ENFORCER_CACHE_EXPIRATION_IN_SEC")
		enforcerCacheExpirationDuration := EnforcerCacheDefaultExpiration
		enforcerCacheExpirationValue, err := strconv.Atoi(enforcerCacheExpirationInSec)
		if err == nil {
			enforcerCacheExpirationDuration = time.Second * time.Duration(enforcerCacheExpirationValue)
		}
		logger.Infow("enforce cache enabled", "expiry", enforcerCacheExpirationDuration)
		return cache.New(enforcerCacheExpirationDuration, 5*time.Minute)
	}
	return nil
}

// Enforcer is a wrapper around an Casbin enforcer that:
// * is backed by a kubernetes config map
// * has a predefined RBAC model
// * supports a built-in policy
// * supports a user-defined bolicy
// * supports a custom JWT claims enforce function
type EnforcerImpl struct {
	lock map[string]*sync.Mutex
	*cache.Cache
	*casbin.Enforcer
	*middleware.SessionManager
	logger *zap.SugaredLogger
}

// Enforce is a wrapper around casbin.Enforce to additionally enforce a default role and a custom
// claims function
func (e *EnforcerImpl) Enforce(rvals ...interface{}) bool {
	return e.enforce(e.Enforcer, rvals...)
}

func (e *EnforcerImpl) EnforceByEmail(rvals ...interface{}) bool {
	return e.enforceByEmail(e.Enforcer, rvals...)
}

// EnforceErr is a convenience helper to wrap a failed enforcement with a detailed error about the request
func (e *EnforcerImpl) EnforceErr(rvals ...interface{}) error {
	if !e.Enforce(rvals...) {
		errMsg := "permission denied"
		if len(rvals) > 0 {
			rvalsStrs := make([]string, len(rvals)-1)
			for i, rval := range rvals[1:] {
				rvalsStrs[i] = fmt.Sprintf("%s", rval)
			}
			errMsg = fmt.Sprintf("%s: %s", errMsg, strings.Join(rvalsStrs, ", "))
		}
		return status.Error(codes.PermissionDenied, errMsg)
	}
	return nil
}

func EnforceByEmailInBatchSync(e *EnforcerImpl, wg *sync.WaitGroup, mutex *sync.RWMutex, result map[string]bool, metrics map[int]int64, index int, emailId string, resource string, action string, vals []string) {
	defer wg.Done()
	start := time.Now()
	batchResult := make(map[string]bool)
	for _, item := range vals {
		batchResult[item] = e.EnforceByEmail(strings.ToLower(emailId), resource, action, item)
	}
	duration := time.Since(start)
	mutex.Lock()
	defer mutex.Unlock()
	for k, v := range batchResult {
		result[k] = v
	}
	metrics[index] = duration.Milliseconds()

}

func (e *EnforcerImpl) EnforceByEmailInBatch(emailId string, resource string, action string, vals []string) map[string]bool {
	var totalTimeGap int64 = 0
	var maxTimegap int64 = 0
	var minTimegap int64 = math.MaxInt64
	var avgTimegap float64
	enforcerMaxBatchSize := os.Getenv("ENFORCER_MAX_BATCH_SIZE")
	batchSize, err := strconv.Atoi(enforcerMaxBatchSize)
	if err != nil {
		batchSize = EnforcerBatchDefaultSize
		err = nil
	}
	var result map[string]bool
	var metrics = make(map[int]int64)

	enforcerCacheMutex := getEnforcerCacheLock(e, emailId)
	enforcerCacheMutex.Lock()
	defer clearCacheLock(e, emailId, enforcerCacheMutex)

	result = getCacheData(e, emailId, resource, action)
	if result != nil {
		e.logger.Infow("enforce request for batch with data from cache", "emailId", emailId, "resource", resource,
			"action", action, "size", len(vals), "cached", "true")

		var newVals []string
		for _, item := range vals {
			_, found := result[item]
			if !found {
				newVals = append(newVals, item)
			}
		}
		vals = newVals
	} else {
		result = make(map[string]bool)
	}

	totalSize := len(vals)
	wg := new(sync.WaitGroup)
	var batchMutex = &sync.RWMutex{}
	if batchSize > totalSize {
		batchSize = totalSize
	}
	wg.Add(batchSize)
	for i := 0; i < batchSize; i++ {
		startIndex := i * totalSize / batchSize
		endIndex := startIndex + totalSize/batchSize
		if endIndex > totalSize {
			endIndex = totalSize
		}
		go EnforceByEmailInBatchSync(e, wg, batchMutex, result, metrics, i, emailId, resource, action, vals[startIndex:endIndex])
	}
	wg.Wait()
	for _, duration := range metrics {
		totalTimeGap += duration
		if duration > maxTimegap {
			maxTimegap = duration
		}
		if duration < minTimegap {
			minTimegap = duration
		}
	}

	storeCacheData(e, emailId, resource, action, result)

	if batchSize > 0 {
		avgTimegap = float64(totalTimeGap / int64(batchSize))
	}
	e.logger.Infow("enforce request for batch with data", "emailId", emailId, "resource", resource,
		"action", action, "totalElapsedTime", totalTimeGap, "maxTimegap", maxTimegap, "minTimegap",
		minTimegap, "avgTimegap", avgTimegap, "size", len(vals), "batchSize", batchSize, "cached", e.Cache != nil)

	return result
}

func getEnforcerCacheLock(e *EnforcerImpl, emailId string) *sync.Mutex {
	enforcerCacheMutex, found := e.lock[getLockKey(emailId)]
	if !found {
		enforcerCacheMutex = &sync.Mutex{}
		e.lock[getLockKey(emailId)] = enforcerCacheMutex
	}
	return enforcerCacheMutex
}

func clearCacheLock(e *EnforcerImpl, emailId string, cacheMutex *sync.Mutex) {
	cacheMutex.Unlock()
	delete(e.lock, getLockKey(emailId))
}

func getCacheData(e *EnforcerImpl, emailId string, resource string, action string) map[string]bool {
	if e.Cache == nil {
		return nil
	}
	emailResult, found := e.Cache.Get(emailId)
	if found {
		e.Cache.Set(emailId, emailResult, cache.DefaultExpiration)
		emailResultMap := emailResult.(map[string]map[string]bool)
		return emailResultMap[getCacheKey(resource, action)]
	}
	return nil
}

func storeCacheData(e *EnforcerImpl, emailId string, resource string, action string, result map[string]bool) {
	if e.Cache == nil {
		return
	}
	emailResult, found := e.Cache.Get(emailId)
	if !found {
		emailResult = make(map[string]map[string]bool)
	}
	emailResult.(map[string]map[string]bool)[getCacheKey(resource, action)] = result
	e.Cache.Set(emailId, emailResult, cache.DefaultExpiration)
}

func getCacheKey(resource string, action string) string {
	return resource + "$$" + action
}

func getLockKey(emailId string) string {
	return emailId
}

func (e *EnforcerImpl) InvalidateCache(emailId string) bool {
	cacheLock := getEnforcerCacheLock(e, emailId)
	cacheLock.Lock()
	defer clearCacheLock(e, emailId, cacheLock)
	if e.Cache != nil {
		e.Cache.Delete(emailId)
		return true
	}
	return false
}

func (e *EnforcerImpl) InvalidateCompleteCache() {
	if e.Cache != nil {
		e.Cache.Flush()
	}
}

// enforce is a helper to additionally check a default role and invoke a custom claims enforcement function
func (e *EnforcerImpl) enforce(enf *casbin.Enforcer, rvals ...interface{}) bool {
	// check the default role
	if len(rvals) == 0 {
		return false
	}
	claims, err := e.SessionManager.VerifyToken(rvals[0].(string))
	if err != nil {
		return false
	}
	mapClaims, err := jwt.MapClaims(claims)
	if err != nil {
		return false
	}
	email := jwt.GetField(mapClaims, "email")
	sub := jwt.GetField(mapClaims, "sub")
	if email == "" && (sub == "admin" || sub == "admin:login") {
		email = "admin"
	}
	rvals[0] = strings.ToLower(email)
	defer handlePanic()
	enforcedStatus := enf.Enforce(rvals...)
	return enforcedStatus
}

// enforce is a helper to additionally check a default role and invoke a custom claims enforcement function
func (e *EnforcerImpl) enforceByEmail(enf *casbin.Enforcer, rvals ...interface{}) bool {
	// check the default role
	if len(rvals) == 0 {
		return false
	}
	defer handlePanic()
	enforcedStatus := enf.Enforce(rvals...)
	return enforcedStatus
}

// MatchKeyByPartFunc is the wrapper of our own customised MatchKeyByPart Func
func MatchKeyByPartFunc(args ...interface{}) (interface{}, error) {
	name1 := args[0].(string)
	name2 := args[1].(string)

	return bool(MatchKeyByPart(name1, name2)), nil
}

// MatchKeyByPart checks whether values in key1 matches all values of key2(values are obtained by splitting key by "/")
// For example - key1 =  "a/b/c" matches key2 = "a/*/c" but not matches for key2 = "a/*/d"
func MatchKeyByPart(key1 string, key2 string) bool {

	if key2 == "*" {
		//policy must be for super-admin role or global-env action
		//no need to check further
		return true
	}

	key1Vals := strings.Split(key1, "/")
	key2Vals := strings.Split(key2, "/")

	if (len(key1Vals) != len(key2Vals)) || len(key1Vals) == 0 {
		//values in keys should be more than zero and must be equal
		return false
	}

	for i, key2Val := range key2Vals {
		key1Val := key1Vals[i]

		if key2Val == "" || key1Val == "" {
			//empty values are not allowed in any key
			return false
		} else {
			// getting index of "*" in key2, will check values of key1 accordingly
			//for example - key2Val = a/bc*/d & key1Val = a/bcd/d, in this case "bc" will be checked in key1Val(upto index of "*")
			j := strings.Index(key2Val, "*")
			if j == -1 {
				if key1Val != key2Val {
					return false
				}
			} else if len(key1Val) > j {
				if key1Val[:j] != key2Val[:j] {
					return false
				}
			} else {
				if key1Val != key2Val[:j] {
					return false
				}
			}
		}
	}
	return true
}
