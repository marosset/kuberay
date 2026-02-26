<!-- markdownlint-disable MD013 -->

# Helm charts release

We host all Helm charts on [kuberay-helm](https://github.com/ray-project/kuberay-helm).
This document describes the process for release managers to release Helm charts.

## The end-to-end workflow

### Step 1: Push a version tag

Once a tag (e.g., `v1.6.0`, `v1.5.2`, `v1.6.0-rc.0`) is pushed in the kuberay repo, the [sync-helm-charts](https://github.com/ray-project/kuberay/blob/master/.github/workflows/sync-helm-charts.yaml)
workflow will run. This workflow will automatically bump Helm chart and image versions and open a PR
in [kuberay-helm](https://github.com/ray-project/kuberay-helm) with those updates, targeting the appropriate branch
(`main` for new releases, `release-X.Y` for patch releases).

The workflow can also be triggered manually via the Actions UI if needed.

### Step 2: Review and merge the PR in kuberay-helm

Review the auto-generated PR in [kuberay-helm](https://github.com/ray-project/kuberay-helm/pulls),
confirm the CI checks pass, and merge it.

### Step 3: Validate the charts

When the PR is merged into `main`, the [chart-releaser-action](https://github.com/ray-project/kuberay-helm/blob/main/.github/workflows/chart-release.yaml)
will create releases and update `index.yaml`.

You can validate the charts as follows:

* Confirm that the [releases](https://github.com/ray-project/kuberay-helm/releases) are created as expected.
* Confirm that [index.yaml](https://github.com/ray-project/kuberay-helm/blob/gh-pages/index.yaml) exists.
* Confirm that [index.yaml](https://github.com/ray-project/kuberay-helm/blob/gh-pages/index.yaml) has the metadata of all releases, including old versions.
* Check the creation/update time of all releases and `index.yaml` to ensure they are updated.

* Install charts from Helm repository.

    ```sh
    helm repo add kuberay https://ray-project.github.io/kuberay-helm/
    helm repo update

    # List all charts
    helm search repo kuberay

    # Install charts (If you want to install charts for a release candidate, add `--version vX.Y.Z-rc.0` to the command below.)
    helm install kuberay-operator kuberay/kuberay-operator
    helm install kuberay-apiserver kuberay/kuberay-apiserver
    helm install raycluster kuberay/ray-cluster
    ```

## Delete the existing releases

`helm/chart-releaser-action` does not encourage users to delete existing releases;
thus, `index.yaml` will not be updated automatically after the deletion.
If you really need to do that, please read this section carefully before you do that.

* Delete the [releases](https://github.com/ray-project/kuberay-helm/releases)
* Remove the related tags using the following command. If tags are not properly removed, you may run into the problem described in [ray-project/kuberay/#561](https://github.com/ray-project/kuberay/issues/561).

    ```sh
    # git remote -v
    # upstream        git@github.com:ray-project/kuberay-helm.git (fetch)
    # upstream        git@github.com:ray-project/kuberay-helm.git (push)

    # The following command deletes the tag "ray-cluster-0.4.0".
    git push --delete upstream ray-cluster-0.4.0
    ```

* Remove `index.yaml`
* Trigger kuberay-helm CI again to create new releases and a new index.yaml.
* Follow "Step 3: Validate the charts" to test it.
