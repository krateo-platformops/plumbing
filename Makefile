# Test targets. `test` runs the fast unit suite; `test-envtest` runs the build-tagged
# functional cache-staleness tests against a real kube-apiserver via controller-runtime/envtest.

SHELL := /usr/bin/env bash

LOCALBIN ?= $(CURDIR)/bin

# The integration suites run `kind create cluster`, which WRITES TO THE DEFAULT KUBECONFIG and
# switches its current-context. On a shared machine that silently repoints every bare kubectl and
# helm command — including other people's, and including ones aimed at a production cluster.
#
# Pointing KUBECONFIG at a scratch file for these targets keeps them out of ~/.kube/config
# entirely: nothing to restore afterwards and no window in which the default means something
# unexpected. The suites themselves are unaffected, because e2e-framework builds each test client
# from `kind get kubeconfig` written to its own temp file rather than from the default.
INTEGRATION_KUBECONFIG ?= $(LOCALBIN)/integration.kubeconfig
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest
# Kubebuilder envtest control-plane (kube-apiserver + etcd) version used by the `-tags envtest` tests.
ENVTEST_K8S_VERSION ?= 1.36.0

.PHONY: test
test: ## Run the unit tests (fast; excludes the envtest-tagged functional tests).
	go test ./... -count=1

.PHONY: test-race
test-race: ## Run the unit tests with the race detector.
	go test ./... -race -count=1

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: setup-envtest
setup-envtest: $(SETUP_ENVTEST) ## Install setup-envtest into ./bin.
$(SETUP_ENVTEST): | $(LOCALBIN)
	# Pin to the controller-runtime 0.22 line (matches sigs.k8s.io/controller-runtime v0.22.3 in
	# go.mod). Do NOT use @latest: newer setup-envtest (v0.24+) requires go >= 1.26 and fails under
	# this module's go 1.25 toolchain (GOTOOLCHAIN=local) with "requires go >= 1.26.0".
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.22

.PHONY: test-envtest
test-envtest: setup-envtest ## Run the envtest (real-apiserver) functional cache-staleness tests.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test -tags envtest ./... -count=1

.PHONY: test-integration
test-integration: ## Run the integration tests (real kind cluster) — helm reconcile no-churn + self-heal.
	# The ONLY tests that exercise Reconcile end to end against a real apiserver, and therefore the
	# only place the apply-if-changed no-churn guarantees are actually proven: the
	# traceparent/tracestate neutralizations (step 4.5), the Secret stringData fold (4.6), adoption
	# of live children missing from the stored manifest (4.7), and the before/after semantic diff
	# that decides whether a revision is written at all (step 6).
	#
	# They sat behind `-tags integration` with no make target and no CI job, so none of it was
	# protected: an edit deleting step 4.5 as redundant would leave CI green and resume per-minute
	# revision churn in production (plumbing#51, finding 1). Step 4.5 is exactly the kind of code
	# that looks redundant to a reader who has not traced why it exists.
	#
	# The suite provisions its own kind cluster through e2e-framework — findOrInstallKind installs
	# the kind binary if absent — so this needs Docker and nothing else. It does NOT tear the
	# cluster down (see TestMain), which is deliberate for local debugging and harmless on an
	# ephemeral CI runner, but a local run leaves a `helm-test` cluster behind.
	#
	# -p 1 is REQUIRED, not tuning. The two integration packages each create their OWN kind cluster
	# — helm/v3 makes "helm-test", endpoints makes "krateo" — and `go test` runs package binaries in
	# PARALLEL by default, so both clusters get built at once. On an 8-CPU/16GB machine that is
	# enough to fail cluster creation outright: measured, a full `./...` run failed in endpoints
	# while endpoints run alone passed in 229s.
	#
	# (An earlier version of this comment said the two packages SHARE one cluster and interfere.
	# They do not — separate clusters, separate names. The fix is the same; the reason is not, and
	# the wrong reason would send the next reader looking for cross-test contamination that cannot
	# happen.)
	#
	# Neither package destroys its cluster at teardown, so both are resident for the rest of the
	# run and on the machine afterwards. That is two clusters' worth of CPU and memory, which is
	# the real resource floor for this target — see test-integration-clean when a machine is
	# already carrying clusters from other work.
	#
	@mkdir -p $(LOCALBIN)
	KUBECONFIG=$(INTEGRATION_KUBECONFIG) go test -tags integration ./... -count=1 -p 1 -timeout 45m

.PHONY: test-integration-clean
test-integration-clean: ## test-integration, but delete the suites' kind clusters first (slower, resource-safe).
	# The suites deliberately leave their clusters running so a failed run can be inspected and the
	# next run starts faster. On a machine already carrying clusters from other work that becomes
	# the failure mode instead: cluster creation fails and the suite reports a setup error rather
	# than a test result. Measured on a machine holding five clusters on 8 CPUs.
	#
	# This target trades the warm-start saving for a guaranteed-clean slate. Prefer plain
	# test-integration; reach for this one when you see `failed to create cluster`.
	@mkdir -p $(LOCALBIN)
	-KUBECONFIG=$(INTEGRATION_KUBECONFIG) kind delete cluster --name helm-test
	-KUBECONFIG=$(INTEGRATION_KUBECONFIG) kind delete cluster --name krateo
	$(MAKE) test-integration

.PHONY: help
help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n",$$1,$$2}'
