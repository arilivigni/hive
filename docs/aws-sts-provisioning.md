# Provisioning AWS STS Clusters

It is possible to use Hive to provision clusters configured to use Amazon's Security Token Service, where cluster components use short lived credentials that are rotated frequently, and the cluster does not have an admin level AWS credential. This feature was added to the in-cluster OpenShift components in 4.7, see documentation [here](https://docs.openshift.com/container-platform/4.7/authentication/managing_cloud_provider_credentials/cco-mode-sts.html).

At present Hive does not automate the STS setup, rather we assume the user configures STS components manually and provides information to Hive. The following instructions refer to the 'ccoctl' tool coming in OpenShift 4.8 with the Cloud Credential Operator. This tool is not yet released but development is underway [here](https://github.com/openshift/cloud-credential-operator) and can be compiled from source, and used to prepare for provisioning 4.7 clusters.

## Extract Credentials Requests from Desired OpenShift Release Image

These vary from release to release, so be sure to use the same release image here as you plan to use for your ClusterDeployment:

```bash
$ mkdir credrequests/
$ oc adm release extract quay.io/openshift-release-dev/ocp-release:4.7.1-x86_64 --credentials-requests --cloud=aws > credrequests/credrequests.yaml
```


## Setup STS Infrastructure

```bash
$ ccoctl create key-pair
$ ccoctl create identity-provider --name-prefix mystsprefix --public-key-file serviceaccount-signer.public --region us-east-1
$ ccoctl create iam-roles --credentials-requests-dir credrequests/ --identity-provider-arn <IdentityProviderARN> --name-prefix mystsprefix --region us-east-1
```

## Create Credentials Secret Manifests

Hive allows passing in arbitrary Kubernetes resource manifests to pass through to the install process. We will leverage this to inject the Secrets and configuration required for an STS cluster.

ccoctl will soon have a command to generate these Secrets but this is not implemented yet. For now you can follow the [manual STS documentation](https://docs.openshift.com/container-platform/4.7/authentication/managing_cloud_provider_credentials/cco-mode-sts.html) for how to create each Secret manually for the CredentialsRequests in your release image.

Use the Role ARN's printed by `ccoctl create iam-roles` and the relevant target Secret namespace/name for each CredentialsRequest you extracted.

Example:

```yaml
apiVersion: v1
stringData:
  credentials: |-
    [default]
    role_arn = arn:aws:iam::125931421481:role/mystsprefix-openshift-image-registry-installer-cloud-credentials
    web_identity_token_file = /var/run/secrets/openshift/serviceaccount/token
kind: Secret
metadata:
  name: installer-cloud-credentials
  namespace: openshift-image-registry
type: Opaque
```


### Create Authentication Manifest

In the same directory as your Secret manifets, add another file named `cluster-authentication-02-config.yaml` to configure the OpenShift Authentication operator to use the S3 OIDC provider created by `ccoctl create identity-provider`.

```yaml
apiVersion: config.openshift.io/v1
kind: Authentication
metadata:
  name: cluster
spec:
  serviceAccountIssuer: https://mystsprefix-oidc.s3.us-east-1.amazonaws.com
```

May also soon be automated by ccoctl.

## Create Hive ClusterDeployment

Create a ClusterDeployment normally with the following changes:

  1. Create a Secret for your private service account signing key created with ccoctl key-pair above: `kubectl create secret generic bound-service-account-signing-key --from-file=bound-service-account-signing-key.key=serviceaccount-signer.private`
  1. Create a ConfigMap for your installer manifets (credential role Secrets, Authentication config): `kubectl create configmap cluster-manifests --from-file=manifests/`
  1. In your InstallConfig set `credentialsMode: Manual`
  1. In your ClusterDeployment set `spec.boundServiceAccountSigningKeySecretRef.name` to point to the Secret created above. (bound-service-account-signing-key)
  1. In your ClusterDeployment set `spec.provisioning.manifestsConfigMapRef` to point to the ConfigMap created above. (cluster-manifests)
  1. Create your ClusterDeployment + InstallConfig to provision your STS cluster.
