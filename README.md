# yeoman

yeoman is an alternative to k8s and Nomad. It aims to be:

1. Simple. Easy to run and understand conceptually from start to finish.
2. Safe. Low-level infrastructure shouldn't be exciting. We write simple code
   that's secure, always safe to upgrade, and offers a LTS release cadence.
3. Batteries included. Everything you need to run highly available, flexible
   architectures at scale.

These are the trade-offs we make to achieve the aforementioned goals:

1. Convention over configuration. There's very few options or ways of doing
   things. You should do things the "Yeoman" way or otherwise use k8s/Nomad.
2. High availability. We'll build a simple server that's easy to bring up,
   understand, and fix if anything goes wrong. There is only one of them, and it
   doesn't scale horizontally. This inherently limits the use of yeoman to
   smaller teams which can organize around brief (< 5 second) outages of yeoman
   for upgrades. Since yeoman isn't in the critical path, no outages affect
   production systems.

## Project status

This is in an extremely early "ideation" stage, well before pre-alpha. It's not
usable yet, and you shouldn't try.

## Goals

* Runs on Google Cloud, but extensible to others. Utilizes Google's
  Container-Optimized OS.
* Runs on any UNIX-like OS. OpenBSD is a first-class citizen with `pledge` and
  `unveil` built in.

## State management

Whereever possible, state is discovered, not stored. This makes it impossible
for the state to go out-of-sync.

Public state, such as the services that should be running and the count for
each of them, is held in a Google Cloud bucket called `yeoman`.

All state, including services that hould be running and the count for each of
them, as well as secrets for your application, are stored in Google Cloud's
Secrets Manager.

## Non-features

These are features we'll avoid.

* Raft / distributed consensus. We'll rethink the architecture such that this
  design is no longer needed and systems can remain simple.
* Critical-path control-plane. The yeoman control plane should be entirely
  removed from the running of services. If yeoman goes down, nothing should be
  affected.
* Agents running on infra.

## Components

* Yeoman itself. This runs as a single binary on a server.
* A reverse proxy and service mesh: @proxy

That's it!

## Getting started

yeoman's core goal is simplicity, and that carries through to its CLI.

* `yeoman init $yeomanIP`:
	* Provide the IP address of the Yeoman service, which all other
	  commands will use.
	* This creates or modifies a `.yeoman.json` file in your current
	  directory.
* `yeoman service list|get|set|delete [$name]`
	* `yeoman -n 3:5 -c $containerName service create $name`:
		* `-n` is the number to run with replication. This is in the
		  form of `min:max` for autoscaling. To disable autoscaling,
		  use a single number, e.g. `-n 3`.
		* `-c` is the container to use.
		* `$name` should include the environment. Good practice is to
		  always append `-$env`, e.g. `dashboard-production` and
		  `dashboard-staging`.
	* `yeoman -n 3:10 -c $containerName2 service update $name`
	* `yeoman service destroy $name`
* `yeoman deploy $serviceName`:
	* Deploys the latest container for the service.
	* Waits for the service to come online and checks health on the new
	  version.
	* If everything is successful, brings down the old version.
	* If everything is not successful, brings down the new version.
	* `$appName` may include the environment, e.g. `dashboard-staging`.
* `yeoman status [$serviceName]`:
	* See yeoman's current view of the world. Which services it's managing,
	  how many exist, what boxes they're hosted on, what IPs they're
	  available through, and their current health.

Since we use Google Cloud secrets, you should manage secrets using `gcloud
secrets` or the web-based UI.

## Advanced commands

* `yeoman clone '*-staging' '$1-qa'`
	* Create a duplicate environment instantly for any services matching
	  the name in question.

## Health checks and auto-scaling

* Each service must implement a healthcheck endpoint:
	* `/health`: respond with 200 OK and a body containing:
		`{"load": 0.3}`, which is what percentage of load the server is
		currently handling. This can be determined by any metric of
		your choosing: HTTP request, "expensive" HTTP requests, etc.
* Services will be scaled up when half of the servers report 0.7 load or
  higher.
* Services will be scaled down when half of the servers report 0.3 load or
  lower.
* Moving averages are calculated over a 60s period, with measurements taken
  every 5s.

To disable auto-scaling for a service, configure `-n $constant` rather than a
range.

## About the name

Since this project is intended in many ways to be the oppositite of "Nomad," I
selected something that connotes that it'll be here a long time.

## Scalability

Instead of one statefile, we could have multiple statefiles -- one per service.
Then locks are only needed on a per-service basis. e.g. a bucket with the
following structure could work:

yeoman:
	- myService
	- mySecondService

Containing version information and what the target states are. When deploying
or modifying the services in any way, Yeoman will acquire an exclusive lock on
that particular service.

Instead of one yeoman server, you could have multiple for redundancy. You just
need individual IPs, since they're coordinated with this distributed lock.

https://github.com/mco-gh/gcslock -> using Google Cloud bucket as a mutex!
	> Need to make it easy to override in the case of a known deadlock,
	> e.g. `-f`.

> Create a bucket in which to store your lock file using the command gsutil mb
> 	gs://your-bucket-name.
> Enable object versioning in your bucket using the command gsutil versioning
> 	set on gs://your-bucket-name.

## Setting up Google

1. Install gcloud.
1. Run: `gcloud auth application-default login`
1. Connect Docker to your Google Container Registry: `gcloud auth configure-docker`

$ gcloud compute instances create ym-abc-1 \
	--machine-type=e2-micro \
	--zone=us-central1-b \
	--image-family cos-stable \
	--image-project cos-cloud \
	--tags=http-server \
	--metadata-from-file user-data=cloud-init
