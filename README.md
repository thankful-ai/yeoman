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

Private state, such as secrets for your application, are stored in Google
Cloud's Secrets Manager.

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
* `yeoman service create|update|destroy $name`
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
	* Adds a service if one doesn't exist, so yeoman will start tracking
	  it.
	* Scales up or down the infra as needed according to the settings.
	* Deploys the latest container.
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
