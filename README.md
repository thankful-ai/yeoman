# yeoman

yeoman is the lazy man's orchestrator.

It's a cloud-only, container-based alternative to k8s, Nomad, and fly that's:

1. **Simple.** Easy to run and understand conceptually from start to finish.
2. **Safe.** Low-level infrastructure shouldn't be exciting. We write as little
   code as possible and make it easy to upgrade while running services.
3. **Secure.** We piggyback on cloud-based IAM to remove trust from the
   equation and make hardened OS and container images the default.

After seven years of managing infrastructure with services used by millions of
people, yeoman represents the sum of all of my learnings. It's the developer
experience I've longed for ever since Heroku was left to rot on the vine.

I hope you love it.


## Project status

This is in an early "pre-alpha" stage. It's usable, but hasn't been tested in
production. Use at your own risk.

Note that only Google Cloud Platform is supported today. Others can be added.
Scratch your itch, submit a PR.


## Architecture

yeoman's server is your control plane. It is isolated from the rest of your
infrastructure, so if yeoman's server goes down, all of your services are
unaffected.

The server tracks two forms of state:

* The desired state:
	* Which services should be running
	* What type of VMs to use
* The current state:
	* What services are running
	* Which VMs actually exist

yeoman's server makes adjustments to your infrastructure as needed to bring the
current state in line with the desired state.

The services that should be running and their configurations are persisted to a
Google Bucket that you define. Each object in the bucket represents one
service.

To deploy, the yeoman client creates or updates a service object in this
bucket. You can thus control access to deploying services using Google's ACL
controls on objects in the bucket: restricting writes on a service file to a
person would prevent them from deploying that service.

The server recognizes when a service object in the bucket has changed and makes
any needed adjustments automatically.

When deploying an update to a service, yeoman will automatically do a highly
available rolling deploy. If the machine specs have not been changed, yeoman
will issue rolling restarts and confirm the service is healthy before
continuing to the next batch. If the machine specs have been changed, yeoman
will do a blue-green deploy: it spins up new boxes, confirms the service
reports healthy on each box, and then shuts down the old.

If anything goes wrong during a deploy, yeoman halts the deploy immediately.
You can fix the issue and redeploy to have yeoman try again.

Each time yeoman creates a VM, yeoman numbers the VM neatly. For example, if
you deploy a service named `dashboard` in triplicate, the VMs will be named
`ym-dashboard-1`, `ym-dashboard-2` and `ym-dashboard-3`. VMs run [Google's
Container-Optimized
OS](https://cloud.google.com/container-optimized-os/docs/concepts/features-and-benefits)
which has strong security guarantees.

VMs are discovered automatically by polling GCP's API. yeoman ignores any VMs
which do not have the prefix `ym-` and thus it is safe to run alongside other
orchestrators or unmanaged VMs.

OCI-compatible containers are built using [Google's minimal distroless
images](https://github.com/GoogleContainerTools/distroless), and yeoman selects
the correct image for the type of app you're deploying. Anything in the
directory with the same name as the service will be bundled into the container
image and pushed to Google's Artifact Registry.

All services that yeoman runs must:
* Connect an HTTP server to port 3000 and respond with `200 OK` to `/health`
  checks.
* Connect to port 3001 if listening directly on TLS.

yeoman works best with the included reverse proxy which also polls GCP's API
and makes each service available with TLS automatically. For more info on this,
see the [proxy
readme](https://github.com/thankful-ai/yeoman/blob/master/cmd/proxy).

It's good practice to keep secrets out of your images. We recommend using
`gcloud secrets` and configure your app to retrieve the secrets in-memory on
boot.


## Getting started

### GCP

* Install `gcloud` and run `gcloud auth application-default login`.
* Create the `yeoman` service agent with the following roles:
	* Editor
	* Artifact Registry Reader
	* Compute Instance Admin (beta)
	* Secret Manager Secret Accessor
* Create 2 separate Google Buckets, each one for your:
	*  TLS certs
	*  Service definition files

### Setup the yeoman server

* Run `yeoman init`. Fill in the initial setup questions.
* This will spin up a
  ~$7/mo VM which runs the yeoman server
  for you. Each time you want to update the yeoman server, you'll re-run this
  command which will safely delete the existing `yeoman` server and create
  another.

### Deploy a proxy and a service

* Install go1.20 or later, then run
  `go install github.com/thankful-ai/yeoman/cmd/yeoman@latest`
* Configure your `yeoman.json` file and one or more app folders which will
  each contain the files you want to copy into your image and `app.json`.
* If you haven't yet, run: `yeoman init` (only need to do this when you want to
  update the yeoman server)
* Run: `yeoman -count 3 -static-ip -http deploy proxy`
* Run: `yeoman -count 3 -machine n2-standard -disk 100 deploy someservice`


## Files

### yeoman.json

When you run `yeoman init`, yeoman will copy the `yeoman.json` file into
your container and use that as its configuration settings.

Example file:

```json
{
    "log": {
        "format": "json",
        "level": "debug"
    },
    "store": "$bucketName",
    "providerRegistries": [
        {
            "provider": "gcp:$projectName:us-central1:us-central1-b",
            "registry": "us-central1-docker.pkg.dev/$projectName/$registryName",
            "serviceAccount": "yeoman@$projectName.iam.gserviceaccount.com"
        }
    ]
}
```

### app.json

When you run `yeoman service deploy foobar`, yeoman looks for the file
`foobar/app.json`. It will use this to build a container image and push the
image to each of the registries you've defined in `yeoman.json`.

There are two keys, `type` and `cmd`. `type` must be one of the following:

* go
* cgo
* python3
* java
* rust
* d
* node

`cmd` is equivalent to the Dockerfile `CMD` instruction and can be copied
verbatim.

Containers built by yeoman will copy everything in the named folder into the
image except for this file, which is used to build the image.

```json
{
	"type": "go",
	"cmd": ["/app/foobar", "-x", "some-opt"]
}
```


## Best Practices

Generally you'll want to create a shell script to handle consistent deployments
and check that file into your source control. For example, let's make
`deploy_dashboard.sh`:

```
#!/usr/bin/env bash
set -eu

name=dashboard
config=$(pwd)/yeoman.json
tmpdir="/tmp/$name"

mkdir "$tmpdir"
go build "./cmd/$name" -o "/tmp/$name"

cd /tmp
yeoman -config "$config" \
	-count 3 \
	-static-ip \
	service deploy "$name"
rm -r "$tmpdir"
```

Then you'll simply run `deploy_dashboard.sh` and yeoman will do a zero-downtime
deploy for you, ensuring that you have (in this example) 3 servers with static
IPs.
