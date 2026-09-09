# ====================================================================================
# Setup Project

# Force -mod=readonly regardless of what an ambient GOFLAGS carries. Without
# this, an inherited GOFLAGS=-mod=mod causes `go tool` invocations later in the
# generate pipeline (goimports resolution walks the full module graph) to
# rewrite go.sum with hashes the pruned file legitimately omits, so
# `make check-diff` fails on a tree nobody edited. -mod=readonly is already
# Go's default; forcing it changes nothing in a correct environment and denies
# a hostile one the ability to rewrite a committed artifact. Any unrelated
# ambient GOFLAGS value is preserved via the filter-out.
export GOFLAGS := -mod=readonly $(filter-out -mod=%,$(GOFLAGS))

PROJECT_NAME := provider-k3s
# UP_ORG is the GitHub org this project is published under -- the same org the
# module path, PROJECT_REPO and the xpkg registry org (below) all name. It is
# a recorded fact, not derived from PROJECT_NAME.
UP_ORG ?= crossplane-contrib
ifeq ($(strip $(UP_ORG)),)
UP_ORG := crossplane-contrib
endif
PROJECT_REPO := github.com/crossplane-contrib/$(PROJECT_NAME)

PLATFORMS ?= linux_amd64 linux_arm64

# build/makelib/common.mk derives VERSION by testing whether `git tag` returns
# anything at all -- an existence check, not a reachability check. A tag that
# is not an ancestor of HEAD (the normal state on a release-X.Y maintenance
# branch once a patch tag has landed there instead of on main) takes the
# `git describe --tags` branch, which cannot describe HEAD, falls back to
# `--always`, and yields a bare SHA instead of a semver string -- which then
# breaks xpkg packaging downstream. This guard reproduces common.mk's own
# tagless derivation whenever no *reachable* tag exists, so VERSION never
# silently becomes a bare SHA. It must stay immediately before the include:
# common.mk only computes VERSION when $(origin VERSION) is still "undefined".
ifeq ($(origin VERSION), undefined)
ifeq ($(shell git describe --tags --abbrev=0 2>/dev/null),)
VERSION := $(shell echo "v0.0.0-$$(git rev-list HEAD --count)-g$$(git describe --dirty --always)" | sed 's/-/./2' | sed 's/-/./2' | sed 's/-/./2')
endif
endif

-include build/makelib/common.mk

# ====================================================================================
# Setup Output

-include build/makelib/output.mk

# ====================================================================================
# Setup Go

NPROCS ?= 1
GO_TEST_PARALLEL := $(shell echo $$(( $(NPROCS) / 2 )))
GO_STATIC_PACKAGES = $(GO_PROJECT)/cmd/provider
GO_REQUIRED_VERSION ?= 1.26
GO_LDFLAGS += -X $(GO_PROJECT)/internal/version.Version=$(VERSION)
GO_SUBDIRS += cmd internal apis
GO111MODULE = on
GOLANGCILINT_VERSION = 2.13.2
-include build/makelib/golang.mk

# ====================================================================================
# Setup Kubernetes tools

-include build/makelib/k8s_tools.mk

# ====================================================================================
# Setup Images

IMAGES = provider-k3s
-include build/makelib/imagelight.mk

# ====================================================================================
# Setup XPKG

XPKG_REG_ORGS ?= xpkg.upbound.io/$(UP_ORG)
# NOTE(hasheddan): skip promoting on xpkg.upbound.io as channel tags are
# inferred.
XPKG_REG_ORGS_NO_PROMOTE ?= xpkg.upbound.io/$(UP_ORG)
XPKGS = provider-k3s
-include build/makelib/xpkg.mk

# ====================================================================================
# Publishing
#
# The crossplane/build xpkg machinery publishes via `crossplane xpkg push`,
# which authenticates through the Docker keychain (~/.docker/config.json). We
# publish with the `up` CLI instead so CI can authenticate with an Upbound API
# token -- the same UP_API_TOKEN / UP_ORG wiring the Upbound configuration
# packages use. Do NOT pass `--create`: it hits the api.upbound.io
# repositories endpoint, and robot tokens 401 there. Registry repositories
# are created out-of-band.
#
# NOTE: the `up login` performed by upbound/action-up is NOT what authenticates
# the push. UP_API_TOKEN is a robot token, and up's RegistryKeychain falls back
# to the Docker keychain for robot tokens ("robot tokens cannot be used for
# registry login 401 Unauthorized"), so CI must additionally `docker login`
# to xpkg.upbound.io with UP_ROBOT_ID as the username. See the "Login to xpkg
# with robot" step in .github/workflows/release.yaml.
UP ?= up

xpkg.push.up: ## Push the built xpkg to xpkg.upbound.io using the up CLI.
	@$(INFO) Pushing package $(XPKG_REG_ORGS)/$(PROJECT_NAME):$(VERSION)
	@$(UP) xpkg push \
		$(foreach p,$(XPKG_LINUX_PLATFORMS),-f $(XPKG_OUTPUT_DIR)/$(p)/$(PROJECT_NAME)-$(VERSION).xpkg) \
		$(XPKG_REG_ORGS)/$(PROJECT_NAME):$(VERSION) || $(FAIL)
	@$(OK) Pushed package $(XPKG_REG_ORGS)/$(PROJECT_NAME):$(VERSION)

.PHONY: xpkg.push.up

BASE_REF ?= origin/main

check-breaking-changes: ## Fail if a changed CRD reshapes a served schema incompatibly
	@BASE_REF=$(BASE_REF) ./hack/check-breaking-changes.sh

# Standalone tool modules (their own go.mod, kept out of GO_SUBDIRS so `make
# test`/`make reviewable` never pull them into the provider's module graph)
# and shell guardrail tests under test/hooks/ have no runner otherwise.
# Discovers modules rather than naming them, and skips package-less ones —
# a stub module holding only go.mod/go.sum makes `go test ./...` exit 1
# ("no packages to test") even though there is nothing to test.
test.tools:
	@rc=0; for d in $$(find tools -name go.mod -not -path '*/vendor/*' -exec dirname {} \; 2>/dev/null); do \
		[ -n "$$(cd $$d && go list ./... 2>/dev/null)" ] || continue; \
		$(INFO) go test $$d; \
		(cd $$d && go test ./... -count=1) || rc=1; \
	done; [ $$rc -eq 0 ] || $(FAIL)
	@for s in test/hooks/*_test.sh; do \
		[ -e "$$s" ] || continue; \
		$(INFO) running $$s; \
		bash "$$s" || exit 1; \
	done
	@$(OK) tool tests passed

reviewable: check-breaking-changes check-conventions test.tools

.PHONY: check-breaking-changes test.tools

# NOTE(hasheddan): we force image building to happen prior to xpkg build so that
# we ensure image is present in daemon.
xpkg.build.provider-k3s: do.build.images

fallthrough: submodules
	@echo Initial setup complete. Running make again . . .
	@make

# Run integration tests.
test-integration: $(KIND) $(KUBECTL) $(CROSSPLANE_CLI) $(HELM3)
	@$(INFO) running integration tests using kind $(KIND_VERSION)
	@KIND_NODE_IMAGE_TAG=${KIND_NODE_IMAGE_TAG} $(ROOT_DIR)/cluster/local/integration_tests.sh || $(FAIL)
	@$(OK) integration tests passed

# End to End testing
CROSSPLANE_VERSION = 2.3.4
CROSSPLANE_CLI_VERSION = v2.3.4
CROSSPLANE_NAMESPACE = crossplane-system
-include build/makelib/local.xpkg.mk
-include build/makelib/controlplane.mk

# uptest fork — carries sidecar-annotation support (harness annotations read
# from a `<manifest>.yaml.uptest` file beside each example instead of inline),
# required the moment any example manifest ships a sidecar: the upstream
# binary k8s_tools.mk pins renders a migrated tree with zero hooks and zero
# annotations at exit 0, so E2E would assert nothing while reporting green.
#
# k8s_tools.mk's own $(TOOLS_HOST_DIR)/uptest-$(UPTEST_VERSION) recipe stays
# untouched — overriding UPTEST_VERSION alone 404s, because its download rule
# hardcodes the upstream org. This block instead points UPTEST at a NEW
# filename under the same $(TOOLS_HOST_DIR) and supplies its own download
# recipe for that filename, so the two never collide on one make target.
# $(TOOLS_HOST_DIR) is hardlinked from a shared cross-worktree tool cache —
# reusing the stock uptest-$(UPTEST_VERSION) path here would swap the binary
# under every other worktree's concurrently-running E2E.
UPTEST_FORK_REPO := kaessert/uptest
UPTEST_FORK_REF  := v2.3.0-fork.e21896e
UPTEST           := $(TOOLS_HOST_DIR)/uptest-fork-$(UPTEST_FORK_REF)

$(UPTEST):
	@$(INFO) installing uptest fork $(UPTEST_FORK_REF)
	@mkdir -p $(TOOLS_HOST_DIR)
	@curl -fsSLo $(UPTEST) https://github.com/$(UPTEST_FORK_REPO)/releases/download/$(UPTEST_FORK_REF)/uptest_$(SAFEHOSTPLATFORM) || $(FAIL)
	@chmod +x $(UPTEST)
	@$(OK) installing uptest fork $(UPTEST_FORK_REF)

UPTEST_EXAMPLE_LIST ?= "examples/namespaced/node.yaml"
uptest: $(UPTEST) $(KUBECTL) $(KIND) $(CHAINSAW) $(CROSSPLANE_CLI)
	@$(INFO) running automated tests
	@KUBECTL=$(KUBECTL) KIND=$(KIND) CHAINSAW=$(CHAINSAW) CROSSPLANE_CLI=$(CROSSPLANE_CLI) CROSSPLANE_NAMESPACE=$(CROSSPLANE_NAMESPACE) KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) $(UPTEST) e2e "$(UPTEST_EXAMPLE_LIST)" --setup-script=cluster/local/setup.sh || $(FAIL)
	@$(OK) running automated tests

local-deploy: build controlplane.up $(YQ)
	$(MAKE) local.xpkg.deploy.provider.$(PROJECT_NAME) DRC_FILE="./examples/deploymentruntimeconfig.yaml" && \
	$(INFO) running locally built provider && \
	$(KUBECTL) wait provider.pkg $(PROJECT_NAME) --for condition=Healthy --timeout 5m && \
	$(KUBECTL) -n $(CROSSPLANE_NAMESPACE) wait --for=condition=Available deployment --all --timeout=5m && \
	$(OK) running locally built provider

e2e: local-deploy uptest

# Update the submodules, such as the common build scripts.
submodules:
	@git submodule sync
	@git submodule update --init --recursive

# NOTE(hasheddan): the build submodule currently overrides XDG_CACHE_HOME in
# order to force the Helm 3 to use the .work/helm directory. This causes Go on
# Linux machines to use that directory as the build cache as well. We should
# adjust this behavior in the build submodule because it is also causing Linux
# users to duplicate their build cache, but for now we just make it easier to
# identify its location in CI so that we cache between builds.
go.cachedir:
	@go env GOCACHE

go.mod.cachedir:
	@go env GOMODCACHE

# NOTE(hasheddan): we must ensure up is installed in tool cache prior to build
# as including the k8s_tools machinery prior to the xpkg machinery sets UP to
# point to tool cache.
build.init: $(CROSSPLANE_CLI)

# This is for running out-of-cluster locally, and is for convenience. Running
# this make target will print out the command which was used. For more control,
# try running the binary directly with different arguments.
run: go.build
	@$(INFO) Running Crossplane locally out-of-cluster . . .
	@# To see other arguments that can be provided, run the command with --help instead
	$(GO_OUT_DIR)/provider --debug

dev: $(KIND) $(KUBECTL)
	@$(INFO) Creating kind cluster
	@$(KIND) create cluster --name=$(PROJECT_NAME)-dev
	@$(KUBECTL) cluster-info --context kind-$(PROJECT_NAME)-dev
	@$(INFO) Installing Provider K3s CRDs
	@$(KUBECTL) apply -R -f package/crds
	@$(INFO) Starting Provider K3s controllers
	@$(GO) run cmd/provider/main.go --debug

dev-clean: $(KIND) $(KUBECTL)
	@$(INFO) Deleting kind cluster
	@$(KIND) delete cluster --name=$(PROJECT_NAME)-dev

.PHONY: submodules fallthrough test-integration run dev dev-clean

# ====================================================================================
# Special Targets

# Install gomplate
GOMPLATE_VERSION := 3.10.0
GOMPLATE := $(TOOLS_HOST_DIR)/gomplate-$(GOMPLATE_VERSION)

$(GOMPLATE):
	@$(INFO) installing gomplate $(SAFEHOSTPLATFORM)
	@mkdir -p $(TOOLS_HOST_DIR)
	@curl -fsSLo $(GOMPLATE) https://github.com/hairyhenderson/gomplate/releases/download/v$(GOMPLATE_VERSION)/gomplate_$(SAFEHOSTPLATFORM) || $(FAIL)
	@chmod +x $(GOMPLATE)
	@$(OK) installing gomplate $(SAFEHOSTPLATFORM)

export GOMPLATE

# This target prepares repo for your provider by replacing all "k3s"
# occurrences with your provider name.
# This target can only be run once, if you want to rerun for some reason,
# consider stashing/resetting your git state.
# Arguments:
#   provider: Camel case name of your provider, e.g. GitHub, PlanetScale
provider.prepare:
	@[ "${provider}" ] || ( echo "argument \"provider\" is not set"; exit 1 )
	@PROVIDER=$(provider) ./hack/helpers/prepare.sh

# This target adds a new api type and its controller.
# You would still need to register new api in "apis/<provider>.go" and
# controller in "internal/controller/<provider>.go".
# Arguments:
#   provider: Camel case name of your provider, e.g. GitHub, PlanetScale
#   group: API group for the type you want to add.
#   kind: Kind of the type you want to add
#	apiversion: API version of the type you want to add. Optional and defaults to "v1alpha1"
provider.addtype: $(GOMPLATE)
	@[ "${provider}" ] || ( echo "argument \"provider\" is not set"; exit 1 )
	@[ "${group}" ] || ( echo "argument \"group\" is not set"; exit 1 )
	@[ "${kind}" ] || ( echo "argument \"kind\" is not set"; exit 1 )
	@PROVIDER=$(provider) GROUP=$(group) KIND=$(kind) APIVERSION=$(apiversion) PROJECT_REPO=$(PROJECT_REPO) ./hack/helpers/addtype.sh

define CROSSPLANE_MAKE_HELP
Crossplane Targets:
    submodules            Update the submodules, such as the common build scripts.
    run                   Run crossplane locally, out-of-cluster. Useful for development.

endef
# The reason CROSSPLANE_MAKE_HELP is used instead of CROSSPLANE_HELP is because the crossplane
# binary will try to use CROSSPLANE_HELP if it is set, and this is for something different.
export CROSSPLANE_MAKE_HELP

crossplane.help:
	@echo "$$CROSSPLANE_MAKE_HELP"

help-special: crossplane.help

vendor: modules.download
vendor.check: modules.check

.PHONY: crossplane.help help-special

check-conventions: ## Detect convention violations (test names, error wrapping, kubectl usage)
	@# PascalCase test names: no underscores after "Test"
	@! grep -rn 'func Test.*_' --include='*_test.go' . 2>/dev/null || \
	  (echo "FAIL: test names must use PascalCase (no underscores)" && exit 1)
	@# KUBECTL env var: test scripts must use $${KUBECTL:-kubectl}, never a bare
	@# `kubectl` invocation. Three passes over each test script keep this to
	@# genuine invocations: comment lines are blanked (not deleted, so line
	@# numbers stay accurate), double-quoted string literals are stripped (so
	@# prose inside a quoted message like `assert_true "... kubectl" "$$rc"`
	@# can't match), and a word boundary is required before `kubectl` (so
	@# `_kubectl` inside a longer identifier, e.g. `__stub_kubectl_empty`,
	@# doesn't match). A `KUBECTL:-kubectl` backstop catches the one shape
	@# quote-stripping and the word boundary both miss: an unquoted
	@# `KUBECTL=$${KUBECTL:-kubectl}` assignment.
	@bad=0; \
	for f in $$(find test/ -name '*.sh' 2>/dev/null); do \
	  m=$$(sed -E 's/^[[:space:]]*#.*$$//' "$$f" | sed -E 's/"[^"]*"//g' \
	    | grep -nE '(^|[^[:alnum:]_$$])kubectl' | grep -v 'KUBECTL:-kubectl'); \
	  [ -z "$$m" ] && continue; \
	  for ln in $$(echo "$$m" | cut -d: -f1); do \
	    echo "$$f:$$ln:$$(sed -n "$${ln}p" "$$f")"; \
	    bad=1; \
	  done; \
	done; \
	if [ "$$bad" -eq 1 ]; then echo "FAIL: use \$${KUBECTL:-kubectl} instead of bare kubectl"; exit 1; fi
	@# Error wrapping: no fmt.Errorf in production code. Comment lines are
	@# filtered out so the controllers' own "never fmt.Errorf" doc comments
	@# do not trip the check that documents them.
	@! grep -rn 'fmt\.Errorf' internal/ --include='*.go' 2>/dev/null \
	    | grep -v '_test.go' \
	    | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' || \
	  (echo "FAIL: use errors.Wrap/Errorf from crossplane-runtime, not fmt.Errorf" && exit 1)
	@# No bare stdlib "errors" import (match import lines only, not JSON struct tags).
	@! grep -rn '^\s*"errors"\s*$$' internal/ --include='*.go' 2>/dev/null | grep -v '_test.go' || \
	  (echo "FAIL: use crossplane-runtime/v2/pkg/errors, not stdlib \"errors\"" && exit 1)
	@echo "check-conventions: all checks passed"

.PHONY: check-conventions
