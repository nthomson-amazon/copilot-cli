// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"path/filepath"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/copilot-cli/internal/pkg/template"
	"github.com/imdario/mergo"
)

const (
	lbWebSvcManifestPath = "workloads/services/lb-web/manifest.yml"

	// LogRetentionInDays is the default log retention time in days.
	LogRetentionInDays = 30
)

// LoadBalancedWebService holds the configuration to build a container image with an exposed port that receives
// requests through a load balancer with AWS Fargate as the compute engine.
type LoadBalancedWebService struct {
	Workload                     `yaml:",inline"`
	LoadBalancedWebServiceConfig `yaml:",inline"`
	// Use *LoadBalancedWebServiceConfig because of https://github.com/imdario/mergo/issues/146
	Environments map[string]*LoadBalancedWebServiceConfig `yaml:",flow"` // Fields to override per environment.

	parser template.Parser
}

// LoadBalancedWebServiceConfig holds the configuration for a load balanced web service.
type LoadBalancedWebServiceConfig struct {
	Image       ServiceImageWithPort `yaml:",flow"`
	RoutingRule `yaml:"http,flow"`
	TaskConfig  `yaml:",inline"`
	*Logging    `yaml:"logging,flow"`
	Sidecar     `yaml:",inline"`
}

// LogConfigOpts converts the service's Firelens configuration into a format parsable by the templates pkg.
func (lc *LoadBalancedWebServiceConfig) LogConfigOpts() *template.LogConfigOpts {
	if lc.Logging == nil {
		return nil
	}
	return lc.logConfigOpts()
}

// RoutingRule holds the path to route requests to the service.
type RoutingRule struct {
	Path            *string `yaml:"path"`
	HealthCheckPath *string `yaml:"healthcheck"`
	Stickiness      *bool   `yaml:"stickiness"`
	// TargetContainer is the container load balancer routes traffic to.
	TargetContainer *string `yaml:"targetContainer"`
}

// LoadBalancedWebServiceProps contains properties for creating a new load balanced fargate service manifest.
type LoadBalancedWebServiceProps struct {
	*WorkloadProps
	Path string
	Port uint16
}

// NewLoadBalancedWebService creates a new public load balanced web service, receives all the requests from the load balancer,
// has a single task with minimal CPU and memory thresholds, and sets the default health check path to "/".
func NewLoadBalancedWebService(props *LoadBalancedWebServiceProps) *LoadBalancedWebService {
	svc := newDefaultLoadBalancedWebService()
	// Apply overrides.
	svc.Name = aws.String(props.Name)
	if props.Dockerfile != "" {
		svc.LoadBalancedWebServiceConfig.Image.Build.BuildArgs.Dockerfile = aws.String(props.Dockerfile)
	} else if props.Builder != "" {
		svc.LoadBalancedWebServiceConfig.Image.Build.BuildArgs.Builder = aws.String(props.Builder)
	}
	svc.LoadBalancedWebServiceConfig.Image.Port = aws.Uint16(props.Port)
	svc.RoutingRule.Path = aws.String(props.Path)
	svc.parser = template.New()
	return svc
}

// newDefaultLoadBalancedWebService returns an empty LoadBalancedWebService with only the default values set.
func newDefaultLoadBalancedWebService() *LoadBalancedWebService {
	return &LoadBalancedWebService{
		Workload: Workload{
			Type: aws.String(LoadBalancedWebServiceType),
		},
		LoadBalancedWebServiceConfig: LoadBalancedWebServiceConfig{
			Image: ServiceImageWithPort{},
			RoutingRule: RoutingRule{
				HealthCheckPath: aws.String("/"),
			},
			TaskConfig: TaskConfig{
				CPU:    aws.Int(256),
				Memory: aws.Int(512),
				Count: Count{
					Value: aws.Int(1),
				},
			},
		},
	}
}

// MarshalBinary serializes the manifest object into a binary YAML document.
// Implements the encoding.BinaryMarshaler interface.
func (s *LoadBalancedWebService) MarshalBinary() ([]byte, error) {
	content, err := s.parser.Parse(lbWebSvcManifestPath, *s, template.WithFuncs(map[string]interface{}{
		"dirName": tplDirName,
	}))
	if err != nil {
		return nil, err
	}
	return content.Bytes(), nil
}

func tplDirName(s string) string {
	return filepath.Dir(s)
}

//BuildArgs returns a docker.BuildArguments object given a ws root directory.
func (s *LoadBalancedWebService) BuildArgs(wsRoot string) *DockerBuildArgs {
	return s.Image.BuildConfig(wsRoot)
}

// ApplyEnv returns the service manifest with environment overrides.
// If the environment passed in does not have any overrides then it returns itself.
func (s LoadBalancedWebService) ApplyEnv(envName string) (*LoadBalancedWebService, error) {
	overrideConfig, ok := s.Environments[envName]
	if !ok {
		return &s, nil
	}
	// Apply overrides to the original service s.
	err := mergo.Merge(&s, LoadBalancedWebService{
		LoadBalancedWebServiceConfig: *overrideConfig,
	}, mergo.WithOverride, mergo.WithOverwriteWithEmptyValue)
	if err != nil {
		return nil, err
	}
	s.Environments = nil
	return &s, nil
}
