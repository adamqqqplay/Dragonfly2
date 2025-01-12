/*
 *     Copyright 2020 The Dragonfly Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

//go:generate mockgen -destination mocks/searcher_mock.go -source searcher.go -package mocks

package searcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/mitchellh/mapstructure"
	"github.com/yl2chen/cidranger"

	logger "d7y.io/dragonfly/v2/internal/dflog"
	"d7y.io/dragonfly/v2/manager/model"
	"d7y.io/dragonfly/v2/pkg/math"
	"d7y.io/dragonfly/v2/pkg/types"
)

const (
	// Condition security domain key.
	ConditionSecurityDomain = "security_domain"

	// Condition IDC key.
	ConditionIDC = "idc"

	// Condition location key.
	ConditionLocation = "location"
)

const (
	// securityDomainAffinityWeight is security domain affinity weight.
	securityDomainAffinityWeight float64 = 0.4

	// cidrAffinityWeight is CIDR affinity weight.
	cidrAffinityWeight float64 = 0.3

	// idcAffinityWeight is IDC affinity weight.
	idcAffinityWeight float64 = 0.15

	// locationAffinityWeight is location affinity weight.
	locationAffinityWeight = 0.1

	// clusterTypeWeight is cluster type weight.
	clusterTypeWeight float64 = 0.05
)

const (
	// Maximum score.
	maxScore float64 = 1.0

	// Minimum score.
	minScore = 0
)

const (
	// Maximum number of elements.
	maxElementLen = 5
)

// Scheduler cluster scopes.
type Scopes struct {
	IDC      string   `mapstructure:"idc"`
	Location string   `mapstructure:"location"`
	CIDRs    []string `mapstructure:"cidrs"`
}

type Searcher interface {
	// FindSchedulerClusters finds scheduler clusters that best matches the evaluation.
	FindSchedulerClusters(ctx context.Context, schedulerClusters []model.SchedulerCluster, ip, hostname string, conditions map[string]string) ([]model.SchedulerCluster, error)
}

type searcher struct {
	cidrs []string
}

func New(pluginDir string) Searcher {
	s, err := LoadPlugin(pluginDir)
	if err != nil {
		logger.Info("use default searcher")
		return &searcher{}
	}

	logger.Info("use searcher plugin")
	return s
}

// FindSchedulerClusters finds scheduler clusters that best matches the evaluation.
func (s *searcher) FindSchedulerClusters(ctx context.Context, schedulerClusters []model.SchedulerCluster, ip, hostname string, conditions map[string]string) ([]model.SchedulerCluster, error) {
	if len(schedulerClusters) <= 0 {
		return nil, errors.New("empty scheduler clusters")
	}

	clusters := FilterSchedulerClusters(conditions, schedulerClusters)
	if len(clusters) == 0 {
		return nil, fmt.Errorf("conditions %#v does not match any scheduler cluster", conditions)
	}

	sort.Slice(
		clusters,
		func(i, j int) bool {
			var si, sj Scopes
			if err := mapstructure.Decode(clusters[i].Scopes, &si); err != nil {
				logger.Errorf("cluster %s decode scopes failed: %v", clusters[i].Name, err)
				return false
			}

			if err := mapstructure.Decode(clusters[j].Scopes, &sj); err != nil {
				logger.Errorf("cluster %s decode scopes failed: %v", clusters[i].Name, err)
				return false
			}

			return Evaluate(ip, hostname, conditions, si, clusters[i]) > Evaluate(ip, hostname, conditions, sj, clusters[j])
		},
	)

	return clusters, nil
}

// Filter the scheduler clusters that dfdaemon can be used.
func FilterSchedulerClusters(conditions map[string]string, schedulerClusters []model.SchedulerCluster) []model.SchedulerCluster {
	var clusters []model.SchedulerCluster
	securityDomain := conditions[ConditionSecurityDomain]
	for _, schedulerCluster := range schedulerClusters {
		// There are no active schedulers in the scheduler cluster
		if len(schedulerCluster.Schedulers) == 0 {
			continue
		}

		// Dfdaemon security_domain does not exist, matching all scheduler clusters
		if securityDomain == "" {
			clusters = append(clusters, schedulerCluster)
			continue
		}

		// Scheduler cluster is default, matching all dfdaemons
		if schedulerCluster.IsDefault {
			clusters = append(clusters, schedulerCluster)
			continue
		}

		// Scheduler cluster SecurityRules does not exist, matching all dfdaemons
		if len(schedulerCluster.SecurityGroup.SecurityRules) == 0 {
			clusters = append(clusters, schedulerCluster)
			continue
		}

		// If security_domain exists for dfdaemon and
		// scheduler cluster SecurityRules also exists,
		// then security_domain and SecurityRules are equal to match.
		for _, securityRule := range schedulerCluster.SecurityGroup.SecurityRules {
			if strings.EqualFold(securityRule.Domain, securityDomain) {
				clusters = append(clusters, schedulerCluster)
			}
		}
	}

	return clusters
}

// Evaluate the degree of matching between scheduler cluster and dfdaemon.
func Evaluate(ip, hostname string, conditions map[string]string, scopes Scopes, cluster model.SchedulerCluster) float64 {
	return securityDomainAffinityWeight*calculateSecurityDomainAffinityScore(conditions[ConditionSecurityDomain], cluster.SecurityGroup.SecurityRules) +
		cidrAffinityWeight*calculateCIDRAffinityScore(ip, scopes.CIDRs) +
		idcAffinityWeight*calculateIDCAffinityScore(conditions[ConditionIDC], scopes.IDC) +
		locationAffinityWeight*calculateMultiElementAffinityScore(conditions[ConditionLocation], scopes.Location) +
		clusterTypeWeight*calculateClusterTypeScore(cluster)
}

// calculateSecurityDomainAffinityScore 0.0~1.0 larger and better.
func calculateSecurityDomainAffinityScore(securityDomain string, securityRules []model.SecurityRule) float64 {
	if securityDomain == "" {
		return minScore
	}

	if len(securityRules) == 0 {
		return minScore
	}

	return maxScore
}

// calculateCIDRAffinityScore 0.0~1.0 larger and better.
func calculateCIDRAffinityScore(ip string, cidrs []string) float64 {
	// Construct CIDR ranger.
	ranger := cidranger.NewPCTrieRanger()
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			logger.Error(err)
			continue
		}

		if err := ranger.Insert(cidranger.NewBasicRangerEntry(*network)); err != nil {
			logger.Error(err)
			continue
		}
	}

	// Determine whether an IP is contained in the constructed networks ranger.
	contains, err := ranger.Contains(net.ParseIP(ip))
	if err != nil {
		logger.Error(err)
		return minScore
	}

	if !contains {
		return minScore
	}

	return maxScore
}

// calculateIDCAffinityScore 0.0~1.0 larger and better.
func calculateIDCAffinityScore(dst, src string) float64 {
	if dst == "" || src == "" {
		return minScore
	}

	if strings.EqualFold(dst, src) {
		return maxScore
	}

	// Dst has only one element, src has multiple elements separated by "|".
	// When dst element matches one of the multiple elements of src,
	// it gets the max score of idc.
	srcElements := strings.Split(src, types.AffinitySeparator)
	for _, srcElement := range srcElements {
		if strings.EqualFold(dst, srcElement) {
			return maxScore
		}
	}

	return minScore
}

// calculateMultiElementAffinityScore 0.0~1.0 larger and better.
func calculateMultiElementAffinityScore(dst, src string) float64 {
	if dst == "" || src == "" {
		return minScore
	}

	if strings.EqualFold(dst, src) {
		return maxScore
	}

	// Calculate the number of multi-element matches divided by "|".
	var score, elementLen int
	dstElements := strings.Split(dst, types.AffinitySeparator)
	srcElements := strings.Split(src, types.AffinitySeparator)
	elementLen = math.Min(len(dstElements), len(srcElements))

	// Maximum element length is 5.
	if elementLen > maxElementLen {
		elementLen = maxElementLen
	}

	for i := 0; i < elementLen; i++ {
		if !strings.EqualFold(dstElements[i], srcElements[i]) {
			break
		}
		score++
	}

	return float64(score) / float64(maxElementLen)
}

// calculateClusterTypeScore 0.0~1.0 larger and better.
func calculateClusterTypeScore(cluster model.SchedulerCluster) float64 {
	if cluster.IsDefault {
		return maxScore
	}

	return minScore
}
