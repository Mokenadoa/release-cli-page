package create

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/api"
	ghContext "github.com/cli/cli/v2/context"
	"github.com/cli/cli/v2/git"
	"github.com/cli/cli/v2/internal/browser"
	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/internal/text"
	"github.com/cli/cli/v2/pkg/cmd/pr/shared"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/cli/cli/v2/pkg/prompt"
	"github.com/spf13/cobra"
)

type iprompter interface {
	Select(string, string, []string) (int, error)
}

type CreateOptions struct {
	// This struct stores user input and factory functions
	HttpClient func() (*http.Client, error)
	GitClient  *git.Client
	Config     func() (config.Config, error)
	IO         *iostreams.IOStreams
	Remotes    func() (ghContext.Remotes, error)
	Branch     func() (string, error)
	Browser    browser.Browser
	Prompter   iprompter
	Finder     shared.PRFinder

	TitleProvided bool
	BodyProvided  bool

	RootDirOverride string
	RepoOverride    string

	Autofill    bool
	WebMode     bool
	RecoverFile string

	IsDraft    bool
	Title      string
	Body       string
	BaseBranch string
	HeadBranch string

	Reviewers []string
	Assignees []string
	Labels    []string
	Projects  []string
	Milestone string

	MaintainerCanModify bool
}

type CreateContext struct {
	// This struct stores contextual data about the creation process and is for building up enough
	// data to create a pull request
	RepoContext        *ghContext.ResolvedRemotes
	BaseRepo           *api.Repository
	HeadRepo           ghrepo.Interface
	BaseTrackingBranch string
	BaseBranch         string
	HeadBranch         string
	HeadBranchLabel    string
	HeadRemote         *ghContext.Remote
	IsPushEnabled      bool
	Client             *api.Client
	GitClient          *git.Client
}

func NewCmdCreate(f *cmdutil.Factory, runF func(*CreateOptions) error) *cobra.Command {
	opts := &CreateOptions{
		IO:         f.IOStreams,
		HttpClient: f.HttpClient,
		GitClient:  f.GitClient,
		Config:     f.Config,
		Remotes:    f.Remotes,
		Branch:     f.Branch,
		Browser:    f.Browser,
		Prompter:   f.Prompter,
	}

	var bodyFile string

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a pull request",
		Long: heredoc.Docf(`
			Create a pull request on GitHub.

			When the current branch isn't fully pushed to a git remote, a prompt will ask where
			to push the branch and offer an option to fork the base repository. Use %[1]s--head%[1]s to
			explicitly skip any forking or pushing behavior.

			A prompt will also ask for the title and the body of the pull request. Use %[1]s--title%[1]s
			and %[1]s--body%[1]s to skip this, or use %[1]s--fill%[1]s to autofill these values from git commits.

			Link an issue to the pull request by referencing the issue in the body of the pull
			request. If the body text mentions %[1]sFixes #123%[1]s or %[1]sCloses #123%[1]s, the referenced issue
			will automatically get closed when the pull request gets merged.

			By default, users with write access to the base repository can push new commits to the
			head branch of the pull request. Disable this with %[1]s--no-maintainer-edit%[1]s.
		`, "`"),
		Example: heredoc.Doc(`
			$ gh pr create --title "The bug is fixed" --body "Everything works again"
			$ gh pr create --reviewer monalisa,hubot  --reviewer myorg/team-name
			$ gh pr create --project "Roadmap"
			$ gh pr create --base develop --head monalisa:feature
		`),
		Args:    cmdutil.NoArgsQuoteReminder,
		Aliases: []string{"new"},
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Finder = shared.NewFinder(f)

			opts.TitleProvided = cmd.Flags().Changed("title")
			opts.RepoOverride, _ = cmd.Flags().GetString("repo")
			noMaintainerEdit, _ := cmd.Flags().GetBool("no-maintainer-edit")
			opts.MaintainerCanModify = !noMaintainerEdit

			if !opts.IO.CanPrompt() && opts.RecoverFile != "" {
				return cmdutil.FlagErrorf("`--recover` only supported when running interactively")
			}

			if opts.IsDraft && opts.WebMode {
				return errors.New("the `--draft` flag is not supported with `--web`")
			}
			if len(opts.Reviewers) > 0 && opts.WebMode {
				return errors.New("the `--reviewer` flag is not supported with `--web`")
			}
			if cmd.Flags().Changed("no-maintainer-edit") && opts.WebMode {
				return errors.New("the `--no-maintainer-edit` flag is not supported with `--web`")
			}

			opts.BodyProvided = cmd.Flags().Changed("body")
			if bodyFile != "" {
				b, err := cmdutil.ReadFile(bodyFile, opts.IO.In)
				if err != nil {
					return err
				}
				opts.Body = string(b)
				opts.BodyProvided = true
			}

			if !opts.IO.CanPrompt() && !opts.WebMode && !opts.Autofill && (!opts.TitleProvided || !opts.BodyProvided) {
				return cmdutil.FlagErrorf("must provide `--title` and `--body` (or `--fill`) when not running interactively")
			}

			if runF != nil {
				return runF(opts)
			}
			return createRun(opts)
		},
	}

	fl := cmd.Flags()
	fl.BoolVarP(&opts.IsDraft, "draft", "d", false, "Mark pull request as a draft")
	fl.StringVarP(&opts.Title, "title", "t", "", "Title for the pull request")
	fl.StringVarP(&opts.Body, "body", "b", "", "Body for the pull request")
	fl.StringVarP(&bodyFile, "body-file", "F", "", "Read body text from `file` (use \"-\" to read from standard input)")
	fl.StringVarP(&opts.BaseBranch, "base", "B", "", "The `branch` into which you want your code merged")
	fl.StringVarP(&opts.HeadBranch, "head", "H", "", "The `branch` that contains commits for your pull request (default: current branch)")
	fl.BoolVarP(&opts.WebMode, "web", "w", false, "Open the web browser to create a pull request")
	fl.BoolVarP(&opts.Autofill, "fill", "f", false, "Do not prompt for title/body and just use commit info")
	fl.StringSliceVarP(&opts.Reviewers, "reviewer", "r", nil, "Request reviews from people or teams by their `handle`")
	fl.StringSliceVarP(&opts.Assignees, "assignee", "a", nil, "Assign people by their `login`. Use \"@me\" to self-assign.")
	fl.StringSliceVarP(&opts.Labels, "label", "l", nil, "Add labels by `name`")
	fl.StringSliceVarP(&opts.Projects, "project", "p", nil, "Add the pull request to projects by `name`")
	fl.StringVarP(&opts.Milestone, "milestone", "m", "", "Add the pull request to a milestone by `name`")
	fl.Bool("no-maintainer-edit", false, "Disable maintainer's ability to modify pull request")
	fl.StringVar(&opts.RecoverFile, "recover", "", "Recover input from a failed run of create")

	return cmd
}

func createRun(opts *CreateOptions) (err error) {
	ctx, err := NewCreateContext(opts)
	if err != nil {
		return
	}

	client := ctx.Client

	state, err := NewIssueState(*ctx, *opts)
	if err != nil {
		return
	}

	var openURL string

	if opts.WebMode {
		if !opts.Autofill {
			state.Title = opts.Title
			state.Body = opts.Body
		}
		err = handlePush(*opts, *ctx)
		if err != nil {
			return
		}
		openURL, err = generateCompareURL(*ctx, *state)
		if err != nil {
			return
		}
		if !shared.ValidURL(openURL) {
			err = fmt.Errorf("cannot open in browser: maximum URL length exceeded")
			return
		}
		return previewPR(*opts, openURL)
	}

	if opts.TitleProvided {
		state.Title = opts.Title
	}

	if opts.BodyProvided {
		state.Body = opts.Body
	}

	existingPR, _, err := opts.Finder.Find(shared.FindOptions{
		Selector:   ctx.HeadBranchLabel,
		BaseBranch: ctx.BaseBranch,
		States:     []string{"OPEN"},
		Fields:     []string{"url"},
	})
	var notFound *shared.NotFoundError
	if err != nil && !errors.As(err, &notFound) {
		return fmt.Errorf("error checking for existing pull request: %w", err)
	}
	if err == nil {
		return fmt.Errorf("a pull request for branch %q into branch %q already exists:\n%s",
			ctx.HeadBranchLabel, ctx.BaseBranch, existingPR.URL)
	}

	message := "\nCreating pull request for %s into %s in %s\n\n"
	if state.Draft {
		message = "\nCreating draft pull request for %s into %s in %s\n\n"
	}

	cs := opts.IO.ColorScheme()

	if opts.IO.CanPrompt() {
		fmt.Fprintf(opts.IO.ErrOut, message,
			cs.Cyan(ctx.HeadBranchLabel),
			cs.Cyan(ctx.BaseBranch),
			ghrepo.FullName(ctx.BaseRepo))
	}

	if opts.Autofill || (opts.TitleProvided && opts.BodyProvided) {
		err = handlePush(*opts, *ctx)
		if err != nil {
			return
		}
		return submitPR(*opts, *ctx, *state)
	}

	if opts.RecoverFile != "" {
		err = shared.FillFromJSON(opts.IO, opts.RecoverFile, state)
		if err != nil {
			return fmt.Errorf("failed to recover input: %w", err)
		}
	}

	if !opts.TitleProvided {
		err = shared.TitleSurvey(state)
		if err != nil {
			return
		}
	}

	editorCommand, err := cmdutil.DetermineEditor(opts.Config)
	if err != nil {
		return
	}

	defer shared.PreserveInput(opts.IO, state, &err)()

	if !opts.BodyProvided {
		templateContent := ""
		if opts.RecoverFile == "" {
			tpl := shared.NewTemplateManager(client.HTTP(), ctx.BaseRepo, opts.RootDirOverride, opts.RepoOverride == "", true)
			var template shared.Template
			template, err = tpl.Choose()
			if err != nil {
				return
			}

			if template != nil {
				templateContent = string(template.Body())
			} else {
				templateContent = string(tpl.LegacyBody())
			}
		}

		err = shared.BodySurvey(state, templateContent, editorCommand)
		if err != nil {
			return
		}
	}

	openURL, err = generateCompareURL(*ctx, *state)
	if err != nil {
		return
	}

	allowPreview := !state.HasMetadata() && shared.ValidURL(openURL)
	allowMetadata := ctx.BaseRepo.ViewerCanTriage()
	action, err := shared.ConfirmPRSubmission(allowPreview, allowMetadata, state.Draft)
	if err != nil {
		return fmt.Errorf("unable to confirm: %w", err)
	}

	if action == shared.MetadataAction {
		fetcher := &shared.MetadataFetcher{
			IO:        opts.IO,
			APIClient: client,
			Repo:      ctx.BaseRepo,
			State:     state,
		}
		err = shared.MetadataSurvey(opts.IO, ctx.BaseRepo, fetcher, state)
		if err != nil {
			return
		}

		action, err = shared.ConfirmPRSubmission(!state.HasMetadata(), false, state.Draft)
		if err != nil {
			return
		}
	}

	if action == shared.CancelAction {
		fmt.Fprintln(opts.IO.ErrOut, "Discarding.")
		err = cmdutil.CancelError
		return
	}

	err = handlePush(*opts, *ctx)
	if err != nil {
		return
	}

	if action == shared.PreviewAction {
		return previewPR(*opts, openURL)
	}

	if action == shared.SubmitDraftAction {
		state.Draft = true
		return submitPR(*opts, *ctx, *state)
	}

	if action == shared.SubmitAction {
		return submitPR(*opts, *ctx, *state)
	}

	err = errors.New("expected to cancel, preview, or submit")
	return
}

func initDefaultTitleBody(ctx CreateContext, state *shared.IssueMetadataState) error {
	baseRef := ctx.BaseTrackingBranch
	headRef := ctx.HeadBranch
	gitClient := ctx.GitClient

	commits, err := gitClient.Commits(context.Background(), baseRef, headRef)
	if err != nil {
		return err
	}

	if len(commits) == 1 {
		state.Title = commits[0].Title
		body, err := gitClient.CommitBody(context.Background(), commits[0].Sha)
		if err != nil {
			return err
		}
		state.Body = body
	} else {
		state.Title = humanize(headRef)

		var body strings.Builder
		for i := len(commits) - 1; i >= 0; i-- {
			fmt.Fprintf(&body, "- %s\n", commits[i].Title)
		}
		state.Body = body.String()
	}

	return nil
}

func determineTrackingBranch(gitClient *git.Client, remotes ghContext.Remotes, headBranch string) *git.TrackingRef {
	refsForLookup := []string{"HEAD"}
	var trackingRefs []git.TrackingRef

	headBranchConfig := gitClient.ReadBranchConfig(context.Background(), headBranch)
	if headBranchConfig.RemoteName != "" {
		tr := git.TrackingRef{
			RemoteName: headBranchConfig.RemoteName,
			BranchName: strings.TrimPrefix(headBranchConfig.MergeRef, "refs/heads/"),
		}
		trackingRefs = append(trackingRefs, tr)
		refsForLookup = append(refsForLookup, tr.String())
	}

	for _, remote := range remotes {
		tr := git.TrackingRef{
			RemoteName: remote.Name,
			BranchName: headBranch,
		}
		trackingRefs = append(trackingRefs, tr)
		refsForLookup = append(refsForLookup, tr.String())
	}

	resolvedRefs, _ := gitClient.ShowRefs(context.Background(), refsForLookup...)
	if len(resolvedRefs) > 1 {
		for _, r := range resolvedRefs[1:] {
			if r.Hash != resolvedRefs[0].Hash {
				continue
			}
			for _, tr := range trackingRefs {
				if tr.String() != r.Name {
					continue
				}
				return &tr
			}
		}
	}

	return nil
}

func NewIssueState(ctx CreateContext, opts CreateOptions) (*shared.IssueMetadataState, error) {
	var milestoneTitles []string
	if opts.Milestone != "" {
		milestoneTitles = []string{opts.Milestone}
	}

	meReplacer := shared.NewMeReplacer(ctx.Client, ctx.BaseRepo.RepoHost())
	assignees, err := meReplacer.ReplaceSlice(opts.Assignees)
	if err != nil {
		return nil, err
	}

	state := &shared.IssueMetadataState{
		Type:       shared.PRMetadata,
		Reviewers:  opts.Reviewers,
		Assignees:  assignees,
		Labels:     opts.Labels,
		Projects:   opts.Projects,
		Milestones: milestoneTitles,
		Draft:      opts.IsDraft,
	}

	if opts.Autofill || !opts.TitleProvided || !opts.BodyProvided {
		err := initDefaultTitleBody(ctx, state)
		if err != nil && opts.Autofill {
			return nil, fmt.Errorf("could not compute title or body defaults: %w", err)
		}
	}

	return state, nil
}

func NewCreateContext(opts *CreateOptions) (*CreateContext, error) {
	httpClient, err := opts.HttpClient()
	if err != nil {
		return nil, err
	}
	client := api.NewClientFromHTTP(httpClient)

	// TODO: consider obtaining remotes from GitClient instead
	remotes, err := opts.Remotes()
	if err != nil {
		// When a repo override value is given, ignore errors when fetching git remotes
		// to support using this command outside of git repos.
		if opts.RepoOverride == "" {
			return nil, err
		}
	}

	repoContext, err := ghContext.ResolveRemotesToRepos(remotes, client, opts.RepoOverride)
	if err != nil {
		return nil, err
	}

	var baseRepo *api.Repository
	if br, err := repoContext.BaseRepo(opts.IO, opts.Prompter); err == nil {
		if r, ok := br.(*api.Repository); ok {
			baseRepo = r
		} else {
			// TODO: if RepoNetwork is going to be requested anyway in `repoContext.HeadRepos()`,
			// consider piggybacking on that result instead of performing a separate lookup
			baseRepo, err = api.GitHubRepo(client, br)
			if err != nil {
				return nil, err
			}
		}
	} else {
		return nil, fmt.Errorf("could not determine base repository: %w", err)
	}

	isPushEnabled := false
	headBranch := opts.HeadBranch
	headBranchLabel := opts.HeadBranch
	if headBranch == "" {
		headBranch, err = opts.Branch()
		if err != nil {
			return nil, fmt.Errorf("could not determine the current branch: %w", err)
		}
		headBranchLabel = headBranch
		isPushEnabled = true
	} else if idx := strings.IndexRune(headBranch, ':'); idx >= 0 {
		headBranch = headBranch[idx+1:]
	}

	gitClient := opts.GitClient
	if ucc, err := gitClient.UncommittedChangeCount(context.Background()); err == nil && ucc > 0 {
		fmt.Fprintf(opts.IO.ErrOut, "Warning: %s\n", text.Pluralize(ucc, "uncommitted change"))
	}

	var headRepo ghrepo.Interface
	var headRemote *ghContext.Remote

	if isPushEnabled {
		// determine whether the head branch is already pushed to a remote
		if pushedTo := determineTrackingBranch(gitClient, remotes, headBranch); pushedTo != nil {
			isPushEnabled = false
			if r, err := remotes.FindByName(pushedTo.RemoteName); err == nil {
				headRepo = r
				headRemote = r
				headBranchLabel = pushedTo.BranchName
				if !ghrepo.IsSame(baseRepo, headRepo) {
					headBranchLabel = fmt.Sprintf("%s:%s", headRepo.RepoOwner(), pushedTo.BranchName)
				}
			}
		}
	}

	// otherwise, ask the user for the head repository using info obtained from the API
	if headRepo == nil && isPushEnabled && opts.IO.CanPrompt() {
		pushableRepos, err := repoContext.HeadRepos()
		if err != nil {
			return nil, err
		}

		if len(pushableRepos) == 0 {
			pushableRepos, err = api.RepoFindForks(client, baseRepo, 3)
			if err != nil {
				return nil, err
			}
		}

		currentLogin, err := api.CurrentLoginName(client, baseRepo.RepoHost())
		if err != nil {
			return nil, err
		}

		hasOwnFork := false
		var pushOptions []string
		for _, r := range pushableRepos {
			pushOptions = append(pushOptions, ghrepo.FullName(r))
			if r.RepoOwner() == currentLogin {
				hasOwnFork = true
			}
		}

		if !hasOwnFork {
			pushOptions = append(pushOptions, "Create a fork of "+ghrepo.FullName(baseRepo))
		}
		pushOptions = append(pushOptions, "Skip pushing the branch")
		pushOptions = append(pushOptions, "Cancel")

		var selectedOption int
		//nolint:staticcheck // SA1019: prompt.SurveyAskOne is deprecated: use Prompter
		err = prompt.SurveyAskOne(&survey.Select{
			Message: fmt.Sprintf("Where should we push the '%s' branch?", headBranch),
			Options: pushOptions,
		}, &selectedOption)
		if err != nil {
			return nil, err
		}

		if selectedOption < len(pushableRepos) {
			headRepo = pushableRepos[selectedOption]
			if !ghrepo.IsSame(baseRepo, headRepo) {
				headBranchLabel = fmt.Sprintf("%s:%s", headRepo.RepoOwner(), headBranch)
			}
		} else if pushOptions[selectedOption] == "Skip pushing the branch" {
			isPushEnabled = false
		} else if pushOptions[selectedOption] == "Cancel" {
			return nil, cmdutil.CancelError
		} else {
			// "Create a fork of ..."
			headBranchLabel = fmt.Sprintf("%s:%s", currentLogin, headBranch)
		}
	}

	if headRepo == nil && isPushEnabled && !opts.IO.CanPrompt() {
		fmt.Fprintf(opts.IO.ErrOut, "aborted: you must first push the current branch to a remote, or use the --head flag")
		return nil, cmdutil.SilentError
	}

	baseBranch := opts.BaseBranch
	if baseBranch == "" {
		baseBranch = baseRepo.DefaultBranchRef.Name
	}
	if headBranch == baseBranch && headRepo != nil && ghrepo.IsSame(baseRepo, headRepo) {
		return nil, fmt.Errorf("must be on a branch named differently than %q", baseBranch)
	}

	baseTrackingBranch := baseBranch
	if baseRemote, err := remotes.FindByRepo(baseRepo.RepoOwner(), baseRepo.RepoName()); err == nil {
		baseTrackingBranch = fmt.Sprintf("%s/%s", baseRemote.Name, baseBranch)
	}

	return &CreateContext{
		BaseRepo:           baseRepo,
		HeadRepo:           headRepo,
		BaseBranch:         baseBranch,
		BaseTrackingBranch: baseTrackingBranch,
		HeadBranch:         headBranch,
		HeadBranchLabel:    headBranchLabel,
		HeadRemote:         headRemote,
		IsPushEnabled:      isPushEnabled,
		RepoContext:        repoContext,
		Client:             client,
		GitClient:          gitClient,
	}, nil

}

func submitPR(opts CreateOptions, ctx CreateContext, state shared.IssueMetadataState) error {
	client := ctx.Client

	params := map[string]interface{}{
		"title":               state.Title,
		"body":                state.Body,
		"draft":               state.Draft,
		"baseRefName":         ctx.BaseBranch,
		"headRefName":         ctx.HeadBranchLabel,
		"maintainerCanModify": opts.MaintainerCanModify,
	}

	if params["title"] == "" {
		return errors.New("pull request title must not be blank")
	}

	err := shared.AddMetadataToIssueParams(client, ctx.BaseRepo, params, &state)
	if err != nil {
		return err
	}

	opts.IO.StartProgressIndicator()
	pr, err := api.CreatePullRequest(client, ctx.BaseRepo, params)
	opts.IO.StopProgressIndicator()
	if pr != nil {
		fmt.Fprintln(opts.IO.Out, pr.URL)
	}
	if err != nil {
		if pr != nil {
			return fmt.Errorf("pull request update failed: %w", err)
		}
		return fmt.Errorf("pull request create failed: %w", err)
	}
	return nil
}

func previewPR(opts CreateOptions, openURL string) error {
	if opts.IO.IsStdinTTY() && opts.IO.IsStdoutTTY() {
		fmt.Fprintf(opts.IO.ErrOut, "Opening %s in your browser.\n", text.DisplayURL(openURL))
	}
	return opts.Browser.Browse(openURL)

}

func handlePush(opts CreateOptions, ctx CreateContext) error {
	didForkRepo := false
	headRepo := ctx.HeadRepo
	headRemote := ctx.HeadRemote
	client := ctx.Client

	var err error
	// if a head repository could not be determined so far, automatically create
	// one by forking the base repository
	if headRepo == nil && ctx.IsPushEnabled {
		opts.IO.StartProgressIndicator()
		headRepo, err = api.ForkRepo(client, ctx.BaseRepo, "", "")
		opts.IO.StopProgressIndicator()
		if err != nil {
			return fmt.Errorf("error forking repo: %w", err)
		}
		didForkRepo = true
	}

	if headRemote == nil && headRepo != nil {
		headRemote, _ = ctx.RepoContext.RemoteForRepo(headRepo)
	}

	// There are two cases when an existing remote for the head repo will be
	// missing:
	// 1. the head repo was just created by auto-forking;
	// 2. an existing fork was discovered by querying the API.
	//
	// In either case, we want to add the head repo as a new git remote so we
	// can push to it.
	if headRemote == nil && ctx.IsPushEnabled {
		cfg, err := opts.Config()
		if err != nil {
			return err
		}
		cloneProtocol, _ := cfg.GetOrDefault(headRepo.RepoHost(), "git_protocol")

		headRepoURL := ghrepo.FormatRemoteURL(headRepo, cloneProtocol)

		// TODO: prevent clashes with another remote of a same name
		gitClient := ctx.GitClient
		gitRemote, err := gitClient.AddRemote(context.Background(), "fork", headRepoURL, []string{})
		if err != nil {
			return fmt.Errorf("error adding remote: %w", err)
		}
		headRemote = &ghContext.Remote{
			Remote: gitRemote,
			Repo:   headRepo,
		}
	}

	// automatically push the branch if it hasn't been pushed anywhere yet
	if ctx.IsPushEnabled {
		pushBranch := func() error {
			pushTries := 0
			maxPushTries := 3
			for {
				w := NewRegexpWriter(opts.IO.ErrOut, gitPushRegexp, "")
				defer w.Flush()
				gitClient := ctx.GitClient
				ref := fmt.Sprintf("HEAD:%s", ctx.HeadBranch)
				if err := gitClient.Push(context.Background(), headRemote.Name, ref, git.WithStderr(w)); err != nil {
					if didForkRepo && pushTries < maxPushTries {
						pushTries++
						// first wait 2 seconds after forking, then 4s, then 6s
						waitSeconds := 2 * pushTries
						fmt.Fprintf(opts.IO.ErrOut, "waiting %s before retrying...\n", text.Pluralize(waitSeconds, "second"))
						time.Sleep(time.Duration(waitSeconds) * time.Second)
						continue
					}
					return err
				}
				break
			}
			return nil
		}

		err := pushBranch()
		if err != nil {
			return err
		}
	}

	return nil
}

func generateCompareURL(ctx CreateContext, state shared.IssueMetadataState) (string, error) {
	u := ghrepo.GenerateRepoURL(
		ctx.BaseRepo,
		"compare/%s...%s?expand=1",
		url.PathEscape(ctx.BaseBranch), url.PathEscape(ctx.HeadBranchLabel))
	url, err := shared.WithPrAndIssueQueryParams(ctx.Client, ctx.BaseRepo, u, state)
	if err != nil {
		return "", err
	}
	return url, nil
}

// Humanize returns a copy of the string s that replaces all instance of '-' and '_' with spaces.
func humanize(s string) string {
	replace := "_-"
	h := func(r rune) rune {
		if strings.ContainsRune(replace, r) {
			return ' '
		}
		return r
	}
	return strings.Map(h, s)
}

var gitPushRegexp = regexp.MustCompile("^remote: (Create a pull request.*by visiting|[[:space:]]*https://.*/pull/new/).*\n?$")
