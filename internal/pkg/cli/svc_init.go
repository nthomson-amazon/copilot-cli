// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/copilot-cli/internal/pkg/aws/sessions"
	"github.com/aws/copilot-cli/internal/pkg/config"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation"
	"github.com/aws/copilot-cli/internal/pkg/docker/dockerfile"
	"github.com/aws/copilot-cli/internal/pkg/manifest"
	"github.com/aws/copilot-cli/internal/pkg/term/color"
	"github.com/aws/copilot-cli/internal/pkg/term/log"
	termprogress "github.com/aws/copilot-cli/internal/pkg/term/progress"
	"github.com/aws/copilot-cli/internal/pkg/term/prompt"
	"github.com/aws/copilot-cli/internal/pkg/term/selector"
	"github.com/aws/copilot-cli/internal/pkg/workspace"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	fmtSvcInitSvcTypePrompt     = "Which %s best represents your service's architecture?"
	fmtSvcInitSvcTypeHelpPrompt = `A %s is a public, internet-facing, HTTP server that's behind a load balancer. 
To learn more see: https://git.io/JfIpv

A %s is a private, non internet-facing service.
To learn more see: https://git.io/JfIpT`

	fmtWkldInitNamePrompt     = "What do you want to %s this %s?"
	fmtWkldInitNameHelpPrompt = `The name will uniquely identify this %s within your app %s.
Deployed resources (such as your ECR repository, logs) will contain this %[1]s's name and be tagged with it.`

	fmtWkldInitDockerfilePrompt      = "Which " + color.Emphasize("Dockerfile") + " would you like to use for %s?"
	wkldInitDockerfileHelpPrompt     = "Dockerfile to use for building your container image."
	fmtWkldInitDockerfilePathPrompt  = "What is the path to the " + color.Emphasize("Dockerfile") + " for %s?"
	wkldInitDockerfilePathHelpPrompt = "Path to Dockerfile to use for building your container image."

	svcInitSvcPortPrompt     = "Which %s do you want customer traffic sent to?"
	svcInitSvcPortHelpPrompt = `The port will be used by the load balancer to route incoming traffic to this service.
You should set this to the port which your Dockerfile uses to communicate with the internet.`

	buildTypeDockerfile = "Dockerfile"
	buildTypeBuildpack  = "Cloud Native Buildpacks"

	buildTypes = []string{
		"Dockerfile",
		"Cloud Native Buildpacks",
	}
)

const (
	fmtAddSvcToAppStart    = "Creating ECR repositories for service %s."
	fmtAddSvcToAppFailed   = "Failed to create ECR repositories for service %s.\n"
	fmtAddSvcToAppComplete = "Created ECR repositories for service %s.\n"
)

const (
	defaultSvcPortString = "80"
	service              = "service"
)

type initSvcVars struct {
	appName          string
	serviceType      string
	name             string
	buildType        string
	dockerfilePath   string
	buildpackBuilder string
	port             uint16
}

type initSvcOpts struct {
	initSvcVars

	// Interfaces to interact with dependencies.
	fs          afero.Fs
	ws          svcDirManifestWriter
	store       store
	appDeployer appDeployer
	prog        progress
	prompt      prompter
	df          dockerfileParser

	sel dockerfileSelector

	// Outputs stored on successful actions.
	manifestPath string

	// sets up Dockerfile parser using fs and input path
	setupParser func(*initSvcOpts)
}

func newInitSvcOpts(vars initSvcVars) (*initSvcOpts, error) {
	store, err := config.NewStore()
	if err != nil {
		return nil, fmt.Errorf("couldn't connect to config store: %w", err)
	}

	ws, err := workspace.New()
	if err != nil {
		return nil, fmt.Errorf("workspace cannot be created: %w", err)
	}

	p := sessions.NewProvider()
	sess, err := p.Default()
	if err != nil {
		return nil, err
	}
	prompter := prompt.New()
	return &initSvcOpts{
		initSvcVars: vars,

		fs:          &afero.Afero{Fs: afero.NewOsFs()},
		store:       store,
		ws:          ws,
		appDeployer: cloudformation.New(sess),
		prog:        termprogress.NewSpinner(),
		prompt:      prompter,
		sel:         selector.NewWorkspaceSelect(prompter, store, ws),

		setupParser: func(o *initSvcOpts) {
			o.df = dockerfile.New(o.fs, o.dockerfilePath)
		},
	}, nil
}

// Validate returns an error if the flag values passed by the user are invalid.
func (o *initSvcOpts) Validate() error {
	if o.appName == "" {
		return errNoAppInWorkspace
	}
	if o.serviceType != "" {
		if err := validateSvcType(o.serviceType); err != nil {
			return err
		}
	}
	if o.name != "" {
		if err := validateSvcName(o.name); err != nil {
			return err
		}
	}
	if o.dockerfilePath != "" {
		if _, err := o.fs.Stat(o.dockerfilePath); err != nil {
			return err
		}
	}
	if o.port != 0 {
		if err := validateSvcPort(o.port); err != nil {
			return err
		}
	}
	return nil
}

// Ask prompts for fields that are required but not passed in.
func (o *initSvcOpts) Ask() error {
	if err := o.askSvcType(); err != nil {
		return err
	}
	if err := o.askSvcName(); err != nil {
		return err
	}
	if err := o.askDockerfile(); err != nil {
		return err
	}
	if err := o.askSvcPort(); err != nil {
		return err
	}

	return nil
}

// Execute writes the service's manifest file and stores the service in SSM.
func (o *initSvcOpts) Execute() error {
	app, err := o.store.GetApplication(o.appName)
	if err != nil {
		return fmt.Errorf("get application %s: %w", o.appName, err)
	}

	manifestPath, err := o.createManifest()
	if err != nil {
		return err
	}
	o.manifestPath = manifestPath

	o.prog.Start(fmt.Sprintf(fmtAddSvcToAppStart, o.name))
	if err := o.appDeployer.AddServiceToApp(app, o.name); err != nil {
		o.prog.Stop(log.Serrorf(fmtAddSvcToAppFailed, o.name))
		return fmt.Errorf("add service %s to application %s: %w", o.name, o.appName, err)
	}
	o.prog.Stop(log.Ssuccessf(fmtAddSvcToAppComplete, o.name))

	if err := o.store.CreateService(&config.Workload{
		App:  o.appName,
		Name: o.name,
		Type: o.serviceType,
	}); err != nil {
		return fmt.Errorf("saving service %s: %w", o.name, err)
	}
	return nil
}

func (o *initSvcOpts) createManifest() (string, error) {
	manifest, err := o.newManifest()
	if err != nil {
		return "", err
	}
	var manifestExists bool
	manifestPath, err := o.ws.WriteServiceManifest(manifest, o.name)
	if err != nil {
		e, ok := err.(*workspace.ErrFileExists)
		if !ok {
			return "", err
		}
		manifestExists = true
		manifestPath = e.FileName
	}
	manifestPath, err = relPath(manifestPath)
	if err != nil {
		return "", err
	}

	manifestMsgFmt := "Wrote the manifest for service %s at %s\n"
	if manifestExists {
		manifestMsgFmt = "Manifest file for service %s already exists at %s, skipping writing it.\n"
	}
	log.Successf(manifestMsgFmt, color.HighlightUserInput(o.name), color.HighlightResource(manifestPath))
	log.Infoln(color.Help(fmt.Sprintf("Your manifest contains configurations like your container size and port (:%d).", o.port)))
	log.Infoln()

	return manifestPath, nil
}

func (o *initSvcOpts) newManifest() (encoding.BinaryMarshaler, error) {
	switch o.serviceType {
	case manifest.LoadBalancedWebServiceType:
		return o.newLoadBalancedWebServiceManifest()
	case manifest.BackendServiceType:
		return o.newBackendServiceManifest()
	default:
		return nil, fmt.Errorf("service type %s doesn't have a manifest", o.serviceType)
	}
}

func (o *initSvcOpts) newLoadBalancedWebServiceManifest() (*manifest.LoadBalancedWebService, error) {
	var err error
	var dfPath string
	if o.dockerfilePath != "" {
		dfPath, err = relativeDockerfilePath(o.ws, o.dockerfilePath)
		if err != nil {
			return nil, err
		}
	}
	props := &manifest.LoadBalancedWebServiceProps{
		WorkloadProps: &manifest.WorkloadProps{
			Name:       o.name,
			Dockerfile: dfPath,
			Builder:    o.buildpackBuilder,
		},
		Port: o.port,
		Path: "/",
	}
	existingSvcs, err := o.store.ListServices(o.appName)
	if err != nil {
		return nil, err
	}
	// We default to "/" for the first service, but if there's another
	// Load Balanced Web Service, we use the svc name as the default, instead.
	for _, existingSvc := range existingSvcs {
		if existingSvc.Type == manifest.LoadBalancedWebServiceType && existingSvc.Name != o.name {
			props.Path = o.name
			break
		}
	}
	return manifest.NewLoadBalancedWebService(props), nil
}

func (o *initSvcOpts) newBackendServiceManifest() (*manifest.BackendService, error) {
	var err error
	var dfPath string
	var hc *manifest.ContainerHealthCheck
	if o.dockerfilePath != "" {
		dfPath, err = relativeDockerfilePath(o.ws, o.dockerfilePath)
		if err != nil {
			return nil, err
		}
		hc, err = o.parseHealthCheck()
		if err != nil {
			return nil, err
		}
	}

	return manifest.NewBackendService(manifest.BackendServiceProps{
		WorkloadProps: manifest.WorkloadProps{
			Name:       o.name,
			Dockerfile: dfPath,
			Builder:    o.buildpackBuilder,
		},
		Port:        o.port,
		HealthCheck: hc,
	}), nil
}

func (o *initSvcOpts) askSvcType() error {
	if o.serviceType != "" {
		return nil
	}

	help := fmt.Sprintf(fmtSvcInitSvcTypeHelpPrompt,
		manifest.LoadBalancedWebServiceType,
		manifest.BackendServiceType,
	)
	msg := fmt.Sprintf(fmtSvcInitSvcTypePrompt, color.Emphasize("service type"))
	t, err := o.prompt.SelectOne(msg, help, manifest.ServiceTypes, prompt.WithFinalMessage("Service type:"))
	if err != nil {
		return fmt.Errorf("select service type: %w", err)
	}
	o.serviceType = t
	return nil
}

func (o *initSvcOpts) askSvcName() error {
	if o.name != "" {
		return nil
	}

	name, err := o.prompt.Get(
		fmt.Sprintf(fmtWkldInitNamePrompt, color.Emphasize("name"), color.HighlightUserInput(o.serviceType)),
		fmt.Sprintf(fmtWkldInitNameHelpPrompt, service, o.appName),
		validateSvcName,
		prompt.WithFinalMessage("Service name:"))
	if err != nil {
		return fmt.Errorf("get service name: %w", err)
	}
	o.name = name
	return nil
}

// askDockerfile prompts for the Dockerfile by looking at sub-directories with a Dockerfile.
func (o *initSvcOpts) askDockerfile() error {
	if o.dockerfilePath != "" && o.buildpackBuilder != "" {
		return fmt.Errorf("cannot specify both dockerfile and buildpack builder")
	}
	if o.dockerfilePath != "" || o.buildpackBuilder != "" {
		return nil
	}

	msg := fmt.Sprintf("How do you want to build your %s?", color.Emphasize("container image"))
	t, err := o.prompt.SelectOne(msg, "help", buildTypes, prompt.WithFinalMessage("Build type:"))
	if err != nil {
		return fmt.Errorf("select service type: %w", err)
	}

	if t == buildTypeBuildpack {
		buildpackBuilder, err := o.prompt.Get(
			fmt.Sprintf("Specify the %s to use", color.Emphasize("builder")),
			"",
			nil,
			prompt.WithFinalMessage("Buildpack builder:"),
			prompt.WithDefaultInput("paketobuildpacks/builder:full"))
		if err != nil {
			return fmt.Errorf("prompt get buildpack builder name: %w", err)
		}
		o.buildpackBuilder = buildpackBuilder
	} else {
		df, err := o.sel.Dockerfile(
			fmt.Sprintf(fmtWkldInitDockerfilePrompt, color.HighlightUserInput(o.name)),
			fmt.Sprintf(fmtWkldInitDockerfilePathPrompt, color.HighlightUserInput(o.name)),
			wkldInitDockerfileHelpPrompt,
			wkldInitDockerfilePathHelpPrompt,
			func(v interface{}) error {
				return validatePath(afero.NewOsFs(), v)
			},
		)
		if err != nil {
			return err
		}
		o.dockerfilePath = df
	}
	return nil
}

func (o *initSvcOpts) askSvcPort() error {
	// Use flag before anything else
	if o.port != 0 {
		return nil
	}

	var defaultPort string

	if o.buildpackBuilder == "" {
		o.setupParser(o)
		ports, err := o.df.GetExposedPorts()
		// Ignore any errors in dockerfile parsing--we'll use the default instead.
		if err != nil {
			log.Debugln(err.Error())
		}

		switch len(ports) {
		case 0:
			// There were no ports detected, keep the default port prompt.
			defaultPort = defaultSvcPortString
		case 1:
			o.port = ports[0]
			return nil
		default:
			defaultPort = strconv.Itoa(int(ports[0]))
		}
	} else {
		defaultPort = defaultSvcPortString
	}

	port, err := o.prompt.Get(
		fmt.Sprintf(svcInitSvcPortPrompt, color.Emphasize("port")),
		svcInitSvcPortHelpPrompt,
		validateSvcPort,
		prompt.WithDefaultInput(defaultPort),
		prompt.WithFinalMessage("Port:"),
	)
	if err != nil {
		return fmt.Errorf("get port: %w", err)
	}

	portUint, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return fmt.Errorf("parse port string: %w", err)
	}

	o.port = uint16(portUint)

	return nil
}

func (o *initSvcOpts) parseHealthCheck() (*manifest.ContainerHealthCheck, error) {
	if o.buildpackBuilder != "" {
		return nil, nil
	}

	o.setupParser(o)
	hc, err := o.df.GetHealthCheck()
	if err != nil {
		return nil, fmt.Errorf("get healthcheck from Dockerfile: %s, %w", o.dockerfilePath, err)
	}
	if hc == nil {
		return nil, nil
	}
	return &manifest.ContainerHealthCheck{
		Interval:    &hc.Interval,
		Timeout:     &hc.Timeout,
		StartPeriod: &hc.StartPeriod,
		Retries:     &hc.Retries,
		Command:     hc.Cmd,
	}, nil
}

// RecommendedActions returns follow-up actions the user can take after successfully executing the command.
func (o *initSvcOpts) RecommendedActions() []string {
	return []string{
		fmt.Sprintf("Update your manifest %s to change the defaults.", color.HighlightResource(o.manifestPath)),
		fmt.Sprintf("Run %s to deploy your service to a %s environment.",
			color.HighlightCode(fmt.Sprintf("copilot svc deploy --name %s --env %s", o.name, defaultEnvironmentName)),
			defaultEnvironmentName),
	}
}

// buildSvcInitCmd build the command for creating a new service.
func buildSvcInitCmd() *cobra.Command {
	vars := initSvcVars{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Creates a new service in an application.",
		Long: `Creates a new service in an application.
This command is also run as part of "copilot init".`,
		Example: `
  Create a "frontend" load balanced web service.
  /code $ copilot svc init --name frontend --svc-type "Load Balanced Web Service" --dockerfile ./frontend/Dockerfile

  Create a "subscribers" backend service.
  /code $ copilot svc init --name subscribers --svc-type "Backend Service"`,
		RunE: runCmdE(func(cmd *cobra.Command, args []string) error {
			opts, err := newInitSvcOpts(vars)
			if err != nil {
				return err
			}
			if err := opts.Validate(); err != nil { // validate flags
				return err
			}
			log.Warningln("It's best to run this command in the root of your workspace.")
			if err := opts.Ask(); err != nil {
				return err
			}
			if err := opts.Execute(); err != nil {
				return err
			}
			log.Infoln("Recommended follow-up actions:")
			for _, followup := range opts.RecommendedActions() {
				log.Infof("- %s\n", followup)
			}
			return nil
		}),
	}
	cmd.Flags().StringVarP(&vars.appName, appFlag, appFlagShort, tryReadingAppName(), appFlagDescription)
	cmd.Flags().StringVarP(&vars.name, nameFlag, nameFlagShort, "", svcFlagDescription)
	cmd.Flags().StringVarP(&vars.serviceType, svcTypeFlag, svcTypeFlagShort, "", svcTypeFlagDescription)
	cmd.Flags().StringVarP(&vars.dockerfilePath, dockerFileFlag, dockerFileFlagShort, "", dockerFileFlagDescription)
	cmd.Flags().StringVarP(&vars.buildpackBuilder, buildpackBuilderFlag, buildpackBuilderFlagShort, "", buildpackBuilderFlagDescription)
	cmd.Flags().Uint16Var(&vars.port, svcPortFlag, 0, svcPortFlagDescription)

	// Bucket flags by service type.
	requiredFlags := pflag.NewFlagSet("Required Flags", pflag.ContinueOnError)
	requiredFlags.AddFlag(cmd.Flags().Lookup(nameFlag))
	requiredFlags.AddFlag(cmd.Flags().Lookup(svcTypeFlag))

	buildFlags := pflag.NewFlagSet("Build Flags", pflag.ContinueOnError)
	requiredFlags.AddFlag(cmd.Flags().Lookup(dockerFileFlag))
	requiredFlags.AddFlag(cmd.Flags().Lookup(buildpackBuilderFlag))

	lbWebSvcFlags := pflag.NewFlagSet(manifest.LoadBalancedWebServiceType, pflag.ContinueOnError)
	lbWebSvcFlags.AddFlag(cmd.Flags().Lookup(svcPortFlag))

	backendSvcFlags := pflag.NewFlagSet(manifest.BackendServiceType, pflag.ContinueOnError)
	backendSvcFlags.AddFlag(cmd.Flags().Lookup(svcPortFlag))

	cmd.Annotations = map[string]string{
		// The order of the sections we want to display.
		"sections":                          fmt.Sprintf(`Required,%s`, strings.Join(manifest.ServiceTypes, ",")),
		"Required":                          requiredFlags.FlagUsages(),
		"Build":                             buildFlags.FlagUsages(),
		manifest.LoadBalancedWebServiceType: lbWebSvcFlags.FlagUsages(),
		manifest.BackendServiceType:         lbWebSvcFlags.FlagUsages(),
	}
	cmd.SetUsageTemplate(`{{h1 "Usage"}}{{if .Runnable}}
  {{.UseLine}}{{end}}{{$annotations := .Annotations}}{{$sections := split .Annotations.sections ","}}{{if gt (len $sections) 0}}

{{range $i, $sectionName := $sections}}{{h1 (print $sectionName " Flags")}}
{{(index $annotations $sectionName) | trimTrailingWhitespaces}}{{if ne (inc $i) (len $sections)}}

{{end}}{{end}}{{end}}{{if .HasAvailableInheritedFlags}}

{{h1 "Global Flags"}}
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasExample}}

{{h1 "Examples"}}{{code .Example}}{{end}}
`)
	return cmd
}
