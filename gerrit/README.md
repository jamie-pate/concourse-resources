# [Gerrit](https://www.gerritcodereview.com/) Resource for [Concourse](https://concourse.ci/)

[![Build Status](https://travis-ci.org/google/concourse-resources.svg?branch=master)](https://travis-ci.org/google/concourse-resources)

Tracks Gerrit change revisions (patch sets).

*This is not an official Google product.*

## Usage

Define a new [resource type](https://concourse.ci/configuring-resource-types.html)
for your pipeline:

``` yaml
resource_types:
- name: gerrit
  type: docker-image
  source:
    repository: us.gcr.io/concourse-resources/gerrit-resource
```

## Source Configuration

* `url`: *Required.* The base URL of the Gerrit REST API.

* `query`: A Gerrit Search query matching desired changes. Defaults to
  `status:open`. You may want to specify a project like:
  `status:open project:my-project`. See Gerrit documentation on
  [Searching Changes](https://gerrit-documentation.storage.googleapis.com/Documentation/2.14.2/user-search.html).

* `cookies`: A string containing cookies in "Netscape cookie file format" (as
  supported by libcurl) to be used when connecting to Gerrit.  Usually used for
  authentication.

* `username`: A username for HTTP Basic authentication to Gerrit.

* `password`: A password for HTTP Basic authentication to Gerrit.

* `digest_auth`: If `true`, use HTTP Digest auth instead of Basic auth.

* `fetch`: If `true`, clone the project into the resource dir. Can be overridden by the `fetch` `in` parameter

* `fetch_protocol`: A protocol name used to resolve a fetch URL for the given
  revision. For more information see the `fetch` field in the
  [Gerrit REST API documenation](https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#revision-info).
  Defaults to `http` or `anonymous http` if available.

* `fetch_url`: A URL to the Gerrit git repository where the given revision can
  be found. Overrides `fetch_protocol`.

* `skip_submodules`: A list of submodules to skip when checking out

## Behavior

### `check`: Check for new revisions.

The Gerrit REST API is queried for revisions created since the given version
was created. If no version is given, the latest revision of the most recently
updated change is returned.

### `in`: Clone the git repository at the given revision.

The repository is cloned and the given revision is checked out.

* `fetch`: Override the source configuration `fetch` parameter.
* `sparse`: List of arguments to pass to `git checkout sparse [...args]`
  * see [git-sparse-checkout set](https://git-scm.com/docs/git-sparse-checkout#Documentation/git-sparse-checkout.txt-set)

A `.gerrit_version.json` file is written with the version info
A `.gerrit_patchset.json` file is written with the patchset info (e.g. `{"change": 1234, "patch_set": 2}`)

#### Parameters

All other parameters are now only set in the source configuration

### `out`

The given revision is updated with the given message and/or label(s).

#### Parameters

* `repository`: *Required.* The directory previously cloned by `in`; usually
  just the resource name.

* `message`: A message to be posted as a comment on the given revision.
  The message can contain build metadata variables. (e.g.: ${BUILD_ID})
  See the [Concourse.CI Metadata Documentation](https://concourse.ci/implementing-resources.html#section_resource-metadata
  for a complete list of variables.

* `message_file`: Path to a file containing a message to be posted as a comment
  on the given revision. This overrides `message` *unless* reading
  `message_file` fails, in which case `message` is used instead. If reading
  `message_file` fails and `message` is not specified then the `put` will fail.
  The message can contain build metadata variables. (e.g.: ${BUILD_ID})
  See the [Concourse.CI Metadata Documentation](https://concourse.ci/implementing-resources.html#section_resource-metadata
  for a complete list of variables.

* `labels`: A map of label names to integers to set on the given revision, e.g.:
  `{Verified: 1}`.

## Example Pipeline

``` yaml
resource_types:
- name: gerrit
  type: docker-image
  source:
    repository: us.gcr.io/concourse-resources/gerrit-resource

resources:
- name: example-gerrit
  type: gerrit
  source:
    url: https://review.example.com
    query: status:open project:example
    cookies: ((gerrit-cookies))
    depth: 1

jobs:
- name: example-ci
  on_failure:
    put: example-gerrit
    params:
      repository: example-gerrit
      message: 'CI failed: ${BUILD_URL}'
      Labels: {Verified: -1}
  on_abort:
    put: example-gerrit
    params:
      repository: example-gerrit
      message: 'CI abort: ${BUILD_URL}'
  on_error:
    put: example-gerrit
    params:
      repository: example-gerrit
      message: CI error! ${BUILD_URL}
      labels: {Verified: -1}
  plan:
  # Trigger this job for every new patch set
  - get: example-gerrit
    version: every
    trigger: true
    params:
      fetch: true
      sparse:
        - /*
        - '!/*/'
        - extradir
  # Push a message to the gerrit changeset and reset the verified label
  # NOTE: this creates a separate resource called 'ci-started' since otherwise
  # it would overwrite the `example-gerrit` resource
  - put: ci-started
    resource: example-gerrit
    params:
      repository: example-gerrit
      message: CI started ${BUILD_URL}
      labels: {Verified: 0}

  - task: example-ci
    file: example-gerrit/ci.yml

  # After a successfuly build, mark the patch set Verified +1
  - put: example-gerrit
    params:
      repository: example-gerrit
      message: CI passed ${BUILD_URL}!
      labels: {Verified: 1}
```
