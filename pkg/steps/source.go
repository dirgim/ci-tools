package steps

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	prowv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/clonerefs"
	"k8s.io/test-infra/prow/pod-utils/decorate"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	buildapi "github.com/openshift/api/build/v1"
	imagev1 "github.com/openshift/api/image/v1"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/results"
	"github.com/openshift/ci-tools/pkg/steps/utils"
)

const (
	CiAnnotationPrefix = "ci.openshift.io"
	JobLabel           = "job"
	BuildIdLabel       = "build-id"
	CreatesLabel       = "creates"
	CreatedByCILabel   = "created-by-ci"

	ProwJobIdLabel = "prow.k8s.io/id"

	gopath        = "/go"
	sshPrivateKey = "/sshprivatekey"
	sshConfig     = "/ssh_config"
	oauthToken    = "/oauth-token"

	OauthSecretKey = "oauth-token"

	PullSecretName = "registry-pull-credentials"
)

type CloneAuthType string

var (
	CloneAuthTypeSSH   CloneAuthType = "SSH"
	CloneAuthTypeOAuth CloneAuthType = "OAuth"
)

type CloneAuthConfig struct {
	Secret *corev1.Secret
	Type   CloneAuthType
}

func (c *CloneAuthConfig) getCloneURI(org, repo string) string {
	if c.Type == CloneAuthTypeSSH {
		return fmt.Sprintf("ssh://git@github.com/%s/%s.git", org, repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", org, repo)
}

var (
	JobSpecAnnotation = fmt.Sprintf("%s/%s", CiAnnotationPrefix, "job-spec")
)

func sourceDockerfile(fromTag api.PipelineImageStreamTagReference, workingDir string, cloneAuthConfig *CloneAuthConfig) string {
	var dockerCommands []string
	var secretPath string

	dockerCommands = append(dockerCommands, "")
	dockerCommands = append(dockerCommands, fmt.Sprintf("FROM %s:%s", api.PipelineImageStream, fromTag))
	dockerCommands = append(dockerCommands, "ADD ./clonerefs /clonerefs")

	if cloneAuthConfig != nil {
		switch cloneAuthConfig.Type {
		case CloneAuthTypeSSH:
			dockerCommands = append(dockerCommands, fmt.Sprintf("ADD %s /etc/ssh/ssh_config", sshConfig))
			dockerCommands = append(dockerCommands, fmt.Sprintf("COPY ./%s %s", corev1.SSHAuthPrivateKey, sshPrivateKey))
			secretPath = sshPrivateKey
		case CloneAuthTypeOAuth:
			dockerCommands = append(dockerCommands, fmt.Sprintf("COPY ./%s %s", OauthSecretKey, oauthToken))
			secretPath = oauthToken
		}
	}

	dockerCommands = append(dockerCommands, fmt.Sprintf("RUN umask 0002 && /clonerefs && find %s/src -type d -not -perm -0775 | xargs --max-procs 10 --max-args 100 --no-run-if-empty chmod g+xw", gopath))
	dockerCommands = append(dockerCommands, fmt.Sprintf("WORKDIR %s/", workingDir))
	dockerCommands = append(dockerCommands, fmt.Sprintf("ENV GOPATH=%s", gopath))

	// After the clonerefs command, we don't need the secret anymore.
	// We don't want to let the key keep existing in the image's layer.
	if len(secretPath) > 0 {
		dockerCommands = append(dockerCommands, fmt.Sprintf("RUN rm -f %s", secretPath))
	}

	dockerCommands = append(dockerCommands, "")

	return strings.Join(dockerCommands, "\n")
}

func defaultPodLabels(jobSpec *api.JobSpec) map[string]string {
	if refs := jobSpec.JobSpec.Refs; refs != nil {
		return trimLabels(map[string]string{
			JobLabel:         jobSpec.Job,
			BuildIdLabel:     jobSpec.BuildID,
			ProwJobIdLabel:   jobSpec.ProwJobID,
			CreatedByCILabel: "true",
			openshiftCIEnv:   "true",
			RefsOrgLabel:     refs.Org,
			RefsRepoLabel:    refs.Repo,
			RefsBranchLabel:  refs.BaseRef,
		})
	}

	if extraRefs := jobSpec.JobSpec.ExtraRefs; len(extraRefs) > 0 {
		return trimLabels(map[string]string{
			JobLabel:         jobSpec.Job,
			BuildIdLabel:     jobSpec.BuildID,
			ProwJobIdLabel:   jobSpec.ProwJobID,
			CreatedByCILabel: "true",
			openshiftCIEnv:   "true",
			RefsOrgLabel:     extraRefs[0].Org,
			RefsRepoLabel:    extraRefs[0].Repo,
			RefsBranchLabel:  extraRefs[0].BaseRef,
		})
	}

	return trimLabels(map[string]string{
		JobLabel:         jobSpec.Job,
		BuildIdLabel:     jobSpec.BuildID,
		ProwJobIdLabel:   jobSpec.ProwJobID,
		CreatedByCILabel: "true",
		openshiftCIEnv:   "true",
	})
}

type sourceStep struct {
	config          api.SourceStepConfiguration
	resources       api.ResourceConfiguration
	client          BuildClient
	jobSpec         *api.JobSpec
	cloneAuthConfig *CloneAuthConfig
	pullSecret      *corev1.Secret
}

func (s *sourceStep) Inputs() (api.InputDefinition, error) {
	return s.jobSpec.Inputs(), nil
}

func (*sourceStep) Validate() error { return nil }

func (s *sourceStep) Run(ctx context.Context) error {
	return results.ForReason("cloning_source").ForError(s.run(ctx))
}

func (s *sourceStep) run(ctx context.Context) error {
	clonerefsRef, err := istObjectReference(ctx, s.client, s.config.ClonerefsImage)
	if err != nil {
		return fmt.Errorf("could not resolve clonerefs source: %w", err)
	}

	return handleBuild(ctx, s.client, createBuild(s.config, s.jobSpec, clonerefsRef, s.resources, s.cloneAuthConfig, s.pullSecret))
}

func createBuild(config api.SourceStepConfiguration, jobSpec *api.JobSpec, clonerefsRef corev1.ObjectReference, resources api.ResourceConfiguration, cloneAuthConfig *CloneAuthConfig, pullSecret *corev1.Secret) *buildapi.Build {
	var refs []prowv1.Refs
	if jobSpec.Refs != nil {
		r := *jobSpec.Refs
		if cloneAuthConfig != nil {
			r.CloneURI = cloneAuthConfig.getCloneURI(r.Org, r.Repo)
		}
		refs = append(refs, r)
	}

	for _, r := range jobSpec.ExtraRefs {
		if cloneAuthConfig != nil {
			r.CloneURI = cloneAuthConfig.getCloneURI(r.Org, r.Repo)
		}
		refs = append(refs, r)
	}

	dockerfile := sourceDockerfile(config.From, decorate.DetermineWorkDir(gopath, refs), cloneAuthConfig)
	buildSource := buildapi.BuildSource{
		Type:       buildapi.BuildSourceDockerfile,
		Dockerfile: &dockerfile,
		Images: []buildapi.ImageSource{
			{
				From: clonerefsRef,
				Paths: []buildapi.ImageSourcePath{
					{
						SourcePath:     config.ClonerefsPath,
						DestinationDir: ".",
					},
				},
			},
		},
	}

	optionsSpec := clonerefs.Options{
		SrcRoot:      gopath,
		Log:          "/dev/null",
		GitUserName:  "ci-robot",
		GitUserEmail: "ci-robot@openshift.io",
		GitRefs:      refs,
		Fail:         true,
	}

	if cloneAuthConfig != nil {
		buildSource.Secrets = append(buildSource.Secrets,
			buildapi.SecretBuildSource{
				Secret: *getSourceSecretFromName(cloneAuthConfig.Secret.Name),
			},
		)
		if cloneAuthConfig.Type == CloneAuthTypeSSH {
			for i, image := range buildSource.Images {
				if image.From == clonerefsRef {
					buildSource.Images[i].Paths = append(buildSource.Images[i].Paths, buildapi.ImageSourcePath{
						SourcePath: sshConfig, DestinationDir: "."})
				}
			}
			optionsSpec.KeyFiles = append(optionsSpec.KeyFiles, sshPrivateKey)
		} else {
			optionsSpec.OauthTokenFile = oauthToken

		}
	}

	optionsJSON, err := clonerefs.Encode(optionsSpec)
	if err != nil {
		panic(fmt.Errorf("couldn't create JSON spec for clonerefs: %w", err))
	}

	build := buildFromSource(jobSpec, config.From, config.To, buildSource, "", resources, pullSecret)
	build.Spec.CommonSpec.Strategy.DockerStrategy.Env = append(
		build.Spec.CommonSpec.Strategy.DockerStrategy.Env,
		corev1.EnvVar{Name: clonerefs.JSONConfigEnvVar, Value: optionsJSON},
	)

	return build
}

func buildFromSource(jobSpec *api.JobSpec, fromTag, toTag api.PipelineImageStreamTagReference, source buildapi.BuildSource, dockerfilePath string, resources api.ResourceConfiguration, pullSecret *corev1.Secret) *buildapi.Build {
	log.Printf("Building %s", toTag)
	buildResources, err := resourcesFor(resources.RequirementsForStep(string(toTag)))
	if err != nil {
		panic(fmt.Errorf("unable to parse resource requirement for build %s: %w", toTag, err))
	}
	var from *corev1.ObjectReference
	if len(fromTag) > 0 {
		from = &corev1.ObjectReference{
			Kind:      "ImageStreamTag",
			Namespace: jobSpec.Namespace(),
			Name:      fmt.Sprintf("%s:%s", api.PipelineImageStream, fromTag),
		}
	}

	layer := buildapi.ImageOptimizationSkipLayers
	labels := defaultPodLabels(jobSpec)
	labels[CreatesLabel] = string(toTag)
	build := &buildapi.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:      string(toTag),
			Namespace: jobSpec.Namespace(),
			Labels:    labels,
			Annotations: map[string]string{
				JobSpecAnnotation: jobSpec.RawSpec(),
			},
		},
		Spec: buildapi.BuildSpec{
			CommonSpec: buildapi.CommonSpec{
				Resources: buildResources,
				Source:    source,
				Strategy: buildapi.BuildStrategy{
					Type: buildapi.DockerBuildStrategyType,
					DockerStrategy: &buildapi.DockerBuildStrategy{
						DockerfilePath:          dockerfilePath,
						From:                    from,
						ForcePull:               true,
						NoCache:                 true,
						Env:                     []corev1.EnvVar{{Name: "BUILD_LOGLEVEL", Value: "0"}}, // this mirrors the default and is done for documentary purposes
						ImageOptimizationPolicy: &layer,
					},
				},
				Output: buildapi.BuildOutput{
					To: &corev1.ObjectReference{
						Kind:      "ImageStreamTag",
						Namespace: jobSpec.Namespace(),
						Name:      fmt.Sprintf("%s:%s", api.PipelineImageStream, toTag),
					},
				},
			},
		},
	}
	if pullSecret != nil {
		build.Spec.Strategy.DockerStrategy.PullSecret = getSourceSecretFromName(PullSecretName)
	}
	if owner := jobSpec.Owner(); owner != nil {
		build.OwnerReferences = append(build.OwnerReferences, *owner)
	}

	addLabelsToBuild(jobSpec.Refs, build, source.ContextDir)
	return build
}

func buildInputsFromStep(inputs map[string]api.ImageBuildInputs) []buildapi.ImageSource {
	var names []string
	for k := range inputs {
		names = append(names, k)
	}
	sort.Strings(names)
	var refs []buildapi.ImageSource
	for _, name := range names {
		value := inputs[name]
		var paths []buildapi.ImageSourcePath
		for _, path := range value.Paths {
			paths = append(paths, buildapi.ImageSourcePath{SourcePath: path.SourcePath, DestinationDir: path.DestinationDir})
		}
		if len(value.As) == 0 && len(paths) == 0 {
			continue
		}
		refs = append(refs, buildapi.ImageSource{
			From: corev1.ObjectReference{
				Kind: "ImageStreamTag",
				Name: fmt.Sprintf("%s:%s", api.PipelineImageStream, name),
			},
			As:    value.As,
			Paths: paths,
		})
	}
	return refs
}

func isBuildPhaseTerminated(phase buildapi.BuildPhase) bool {
	switch phase {
	case buildapi.BuildPhaseNew,
		buildapi.BuildPhasePending,
		buildapi.BuildPhaseRunning:
		return false
	}
	return true
}

func handleBuild(ctx context.Context, buildClient BuildClient, build *buildapi.Build) error {
	if err := buildClient.Create(ctx, build); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return fmt.Errorf("could not create build %s: %w", build.Name, err)
		}
		b := &buildapi.Build{}
		if err := buildClient.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: build.Namespace, Name: build.Name}, b); err != nil {
			return fmt.Errorf("could not get build %s: %w", build.Name, err)
		}

		if isBuildPhaseTerminated(b.Status.Phase) &&
			(isInfraReason(b.Status.Reason) || hintsAtInfraReason(b.Status.LogSnippet)) {
			log.Printf("Build %s previously failed from an infrastructure error (%s), retrying...\n", b.Name, b.Status.Reason)
			zero := int64(0)
			foreground := metav1.DeletePropagationForeground
			opts := metav1.DeleteOptions{
				GracePeriodSeconds: &zero,
				Preconditions:      &metav1.Preconditions{UID: &b.UID},
				PropagationPolicy:  &foreground,
			}
			if err := buildClient.Delete(ctx, build, &ctrlruntimeclient.DeleteOptions{Raw: &opts}); err != nil && !kerrors.IsNotFound(err) && !kerrors.IsConflict(err) {
				return fmt.Errorf("could not delete build %s: %w", build.Name, err)
			}
			if err := waitForBuildDeletion(ctx, buildClient, build.Namespace, build.Name); err != nil {
				return fmt.Errorf("could not wait for build %s to be deleted: %w", build.Name, err)
			}
			if err := buildClient.Create(ctx, build); err != nil && !kerrors.IsAlreadyExists(err) {
				return fmt.Errorf("could not recreate build %s: %w", build.Name, err)
			}
		}
	}
	err := waitForBuildOrTimeout(ctx, buildClient, build.Namespace, build.Name)
	if err == nil {
		if err := gatherSuccessfulBuildLog(buildClient, build.Namespace, build.Name); err != nil {
			// log error but do not fail successful build
			log.Printf("problem gathering successful build %s logs into artifacts: %v", build.Name, err)
		}
	}
	// this will still be the err from waitForBuild
	return err

}

func waitForBuildDeletion(ctx context.Context, client ctrlruntimeclient.Client, ns, name string) error {
	ch := make(chan error)
	go func() {
		ch <- wait.ExponentialBackoff(wait.Backoff{
			Duration: 10 * time.Millisecond, Factor: 2, Steps: 10,
		}, func() (done bool, err error) {
			if err := client.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: ns, Name: name}, &buildapi.Build{}); err != nil {
				if kerrors.IsNotFound(err) {
					return true, nil
				}
				return false, err
			}
			return false, nil
		})
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-ch:
		return err
	}
}

func isInfraReason(reason buildapi.StatusReason) bool {
	infraReasons := []buildapi.StatusReason{
		buildapi.StatusReasonCannotCreateBuildPod,
		buildapi.StatusReasonBuildPodDeleted,
		buildapi.StatusReasonExceededRetryTimeout,
		buildapi.StatusReasonPushImageToRegistryFailed,
		buildapi.StatusReasonPullBuilderImageFailed,
		buildapi.StatusReasonFetchSourceFailed,
		buildapi.StatusReasonBuildPodExists,
		buildapi.StatusReasonNoBuildContainerStatus,
		buildapi.StatusReasonFailedContainer,
		buildapi.StatusReasonOutOfMemoryKilled,
		buildapi.StatusReasonCannotRetrieveServiceAccount,
		buildapi.StatusReasonFetchImageContentFailed,
		buildapi.StatusReason("BuildPodEvicted"), // vendoring to get this is so hard
	}
	for _, option := range infraReasons {
		if reason == option {
			return true
		}
	}
	return false
}

func hintsAtInfraReason(logSnippet string) bool {
	return strings.Contains(logSnippet, "error: build error: no such image") ||
		strings.Contains(logSnippet, "[Errno 256] No more mirrors to try.") ||
		strings.Contains(logSnippet, "Error: Failed to synchronize cache for repo") ||
		strings.Contains(logSnippet, "Could not resolve host: ") ||
		strings.Contains(logSnippet, "net/http: TLS handshake timeout") ||
		strings.Contains(logSnippet, "All mirrors were tried") ||
		strings.Contains(logSnippet, "connection reset by peer")
}

func waitForBuildOrTimeout(ctx context.Context, buildClient BuildClient, namespace, name string) error {
	isOK := func(b *buildapi.Build) bool {
		return b.Status.Phase == buildapi.BuildPhaseComplete
	}
	isFailed := func(b *buildapi.Build) bool {
		return b.Status.Phase == buildapi.BuildPhaseFailed ||
			b.Status.Phase == buildapi.BuildPhaseCancelled ||
			b.Status.Phase == buildapi.BuildPhaseError
	}

	build := &buildapi.Build{}
	if err := buildClient.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: namespace, Name: name}, build); err != nil {
		if kerrors.IsNotFound(err) {
			return fmt.Errorf("could not find build %s", name)
		}
		return fmt.Errorf("could not get build: %w", err)
	}
	if isOK(build) {
		log.Printf("Build %s already succeeded in %s", build.Name, buildDuration(build))
		return nil
	}
	if isFailed(build) {
		log.Printf("Build %s failed, printing logs:", build.Name)
		printBuildLogs(buildClient, build.Namespace, build.Name)
		return appendLogToError(fmt.Errorf("the build %s failed with reason %s: %s", build.Name, build.Status.Reason, build.Status.Message), build.Status.LogSnippet)
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := buildClient.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: namespace, Name: name}, build); err != nil {
				log.Printf("Failed to get build %s: %v", name, err)
				continue
			}
			if isOK(build) {
				log.Printf("Build %s succeeded after %s", build.Name, buildDuration(build).Truncate(time.Second))
				return nil
			}
			if isFailed(build) {
				log.Printf("Build %s failed, printing logs:", build.Name)
				printBuildLogs(buildClient, build.Namespace, build.Name)
				return appendLogToError(fmt.Errorf("the build %s failed after %s with reason %s: %s", build.Name, buildDuration(build).Truncate(time.Second), build.Status.Reason, build.Status.Message), build.Status.LogSnippet)
			}
		}
	}
}

func appendLogToError(err error, log string) error {
	log = strings.TrimSpace(log)
	if len(log) == 0 {
		return err
	}
	return fmt.Errorf("%s\n\n%s", err.Error(), log)
}

func buildDuration(build *buildapi.Build) time.Duration {
	start := build.Status.StartTimestamp
	if start == nil {
		start = &build.CreationTimestamp
	}
	end := build.Status.CompletionTimestamp
	if end == nil {
		end = &metav1.Time{Time: time.Now()}
	}
	duration := end.Sub(start.Time)
	return duration
}

func printBuildLogs(buildClient BuildClient, namespace, name string) {
	if s, err := buildClient.Logs(namespace, name, &buildapi.BuildLogOptions{
		NoWait: true,
	}); err == nil {
		defer s.Close()
		if _, err := io.Copy(os.Stdout, s); err != nil {
			log.Printf("error: Unable to copy log output from failed build: %v", err)
		}
	} else {
		log.Printf("error: Unable to retrieve logs from failed build: %v", err)
	}
}

func resourcesFor(req api.ResourceRequirements) (corev1.ResourceRequirements, error) {
	apireq := corev1.ResourceRequirements{}
	for name, value := range req.Requests {
		q, err := resource.ParseQuantity(value)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("invalid resource request: %w", err)
		}
		if apireq.Requests == nil {
			apireq.Requests = make(corev1.ResourceList)
		}
		apireq.Requests[corev1.ResourceName(name)] = q
	}
	for name, value := range req.Limits {
		q, err := resource.ParseQuantity(value)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("invalid resource limit: %w", err)
		}
		if apireq.Limits == nil {
			apireq.Limits = make(corev1.ResourceList)
		}
		apireq.Limits[corev1.ResourceName(name)] = q
	}
	return apireq, nil
}

func (s *sourceStep) Requires() []api.StepLink {
	return []api.StepLink{api.InternalImageLink(s.config.From)}
}

func (s *sourceStep) Creates() []api.StepLink {
	return []api.StepLink{api.InternalImageLink(s.config.To)}
}

func (s *sourceStep) Provides() api.ParameterMap {
	return api.ParameterMap{
		utils.PipelineImageEnvFor(s.config.To): utils.ImageDigestFor(s.client, s.jobSpec.Namespace, api.PipelineImageStream, string(s.config.To)),
	}
}

func (s *sourceStep) Name() string { return string(s.config.To) }

func (s *sourceStep) Description() string {
	return fmt.Sprintf("Clone the correct source code into an image and tag it as %s", s.config.To)
}

func (s *sourceStep) Objects() []ctrlruntimeclient.Object {
	return s.client.Objects()
}

func SourceStep(config api.SourceStepConfiguration, resources api.ResourceConfiguration, buildClient BuildClient,
	jobSpec *api.JobSpec, cloneAuthConfig *CloneAuthConfig, pullSecret *corev1.Secret) api.Step {
	return &sourceStep{
		config:          config,
		resources:       resources,
		client:          buildClient,
		jobSpec:         jobSpec,
		cloneAuthConfig: cloneAuthConfig,
		pullSecret:      pullSecret,
	}
}

// trimLabels ensures that all label values are less than 64 characters
// in length and thus valid.
func trimLabels(labels map[string]string) map[string]string {
	for k, v := range labels {
		if len(v) > 63 {
			labels[k] = v[:60] + "XXX"
		}
	}
	return labels
}

func getSourceSecretFromName(secretName string) *corev1.LocalObjectReference {
	if len(secretName) == 0 {
		return nil
	}
	return &corev1.LocalObjectReference{Name: secretName}
}

func getReasonsForUnreadyContainers(p *corev1.Pod) string {
	builder := &strings.Builder{}
	for _, c := range p.Status.ContainerStatuses {
		if c.Ready {
			continue
		}
		var reason, message string
		switch {
		case c.State.Waiting != nil:
			reason = c.State.Waiting.Reason
			message = c.State.Waiting.Message
		case c.State.Running != nil:
			reason = c.State.Waiting.Reason
			message = c.State.Waiting.Message
		case c.State.Terminated != nil:
			reason = c.State.Terminated.Reason
			message = c.State.Terminated.Message
		default:
			reason = "unknown"
			message = "unknown"
		}
		if message != "" {
			message = fmt.Sprintf(" and message %s", message)
		}
		_, _ = builder.WriteString(fmt.Sprintf("\n* Container %s is not ready with reason %s%s", c.Name, reason, message))
	}
	return builder.String()
}

func getEventsForPod(ctx context.Context, pod *corev1.Pod, client ctrlruntimeclient.Client) string {
	events := &corev1.EventList{}
	listOpts := &ctrlruntimeclient.ListOptions{
		Namespace:     pod.Namespace,
		FieldSelector: fields.OneTermEqualSelector("involvedObject.uid", string(pod.GetUID())),
	}
	if err := client.List(ctx, events, listOpts); err != nil {
		log.Printf("Could not fetch events: %v", err)
		return ""
	}
	builder := &strings.Builder{}
	_, _ = builder.WriteString(fmt.Sprintf("Found %d events for Pod %s:", len(events.Items), pod.Name))
	for _, event := range events.Items {
		_, _ = builder.WriteString(fmt.Sprintf("\n* %dx %s: %s", event.Count, event.Source.Component, event.Message))
	}
	return builder.String()
}

func addLabelsToBuild(refs *prowv1.Refs, build *buildapi.Build, contextDir string) {
	labels := make(map[string]string)
	// reset all labels that may be set by a lower level
	for _, key := range []string{
		"vcs-type",
		"vcs-ref",
		"vcs-url",
		"io.openshift.build.name",
		"io.openshift.build.namespace",
		"io.openshift.build.commit.id",
		"io.openshift.build.commit.ref",
		"io.openshift.build.commit.message",
		"io.openshift.build.commit.author",
		"io.openshift.build.commit.date",
		"io.openshift.build.source-location",
		"io.openshift.build.source-context-dir",
	} {
		labels[key] = ""
	}
	if refs != nil {
		if len(refs.Pulls) == 0 {
			labels["vcs-type"] = "git"
			labels["vcs-ref"] = refs.BaseSHA
			labels["io.openshift.build.commit.id"] = refs.BaseSHA
			labels["io.openshift.build.commit.ref"] = refs.BaseRef
			labels["vcs-url"] = fmt.Sprintf("https://github.com/%s/%s", refs.Org, refs.Repo)
			labels["io.openshift.build.source-location"] = labels["vcs-url"]
			labels["io.openshift.build.source-context-dir"] = contextDir
		}
		// TODO: we should consider setting enough info for a caller to reconstruct pulls to support
		// oc adm release info tooling
	}

	for k, v := range labels {
		build.Spec.Output.ImageLabels = append(build.Spec.Output.ImageLabels, buildapi.ImageLabel{
			Name:  k,
			Value: v,
		})
	}
	sort.Slice(build.Spec.Output.ImageLabels, func(i, j int) bool {
		return build.Spec.Output.ImageLabels[i].Name < build.Spec.Output.ImageLabels[j].Name
	})
}

func istObjectReference(ctx context.Context, client ctrlruntimeclient.Client, reference api.ImageStreamTagReference) (corev1.ObjectReference, error) {
	is := &imagev1.ImageStream{}
	if err := client.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: reference.Namespace, Name: reference.Name}, is); err != nil {
		return corev1.ObjectReference{}, fmt.Errorf("could not resolve remote image stream: %w", err)
	}
	var repo string
	if len(is.Status.PublicDockerImageRepository) > 0 {
		repo = is.Status.PublicDockerImageRepository
	} else if len(is.Status.DockerImageRepository) > 0 {
		repo = is.Status.DockerImageRepository
	} else {
		return corev1.ObjectReference{}, fmt.Errorf("remote image stream %s has no accessible image registry value", reference.Name)
	}
	ist := &imagev1.ImageStreamTag{}
	if err := client.Get(ctx, ctrlruntimeclient.ObjectKey{
		Namespace: reference.Namespace,
		Name:      fmt.Sprintf("%s:%s", reference.Name, reference.Tag),
	}, ist); err != nil {
		return corev1.ObjectReference{}, fmt.Errorf("could not resolve remote image stream tag: %w", err)
	}
	return corev1.ObjectReference{Kind: "DockerImage", Name: fmt.Sprintf("%s@%s", repo, ist.Image.Name)}, nil
}
