# proxy

proxy is a reverse proxy designed to be used with yeoman.

Like yeoman, it maintains no state. Instead it frequently retrieves GCP's VMs
via the API, and sets up new routes for you automatically. Routes are always
the same as the service name, e.g. if you have a yeoman service `abc`, VMs
`ym-abc-1` and `ym-abc-2`, and a host `example.com`, proxy will automatically
route `abc.example.com` to both of the VMs using random routing.

proxy does health checks for every service. Every service *must* respond to
`GET /health` with `200 OK`.

Servers which do not respond to healthchecks with `200 OK` are considered down
and removed from routing. They will automatically become available again when
they come back online.

To see what backends are marked as available, you can access a `/services`
endpoint from an allowed subnet. For example:

```
$ gcloud compute ssh ym-proxy-1
> curl 127.0.0.1/services
{"my-service.example.com": ["100.0.0.1", "100.0.0.2"]}
```

Note that the proxy does not healthcheck itself or other proxy servers. You
should use an external status monitoring tool to check the health of your
services and through which your proxy will either be clearly available or not.

proxy uses letsencrypt to automatically generate and manage TLS certificates
for you. Certificates are stored in a bucket, and it's safe to run the proxy
with redundancy.


## Configuration

Like all other yeoman services, you'll need an `app.json` file:

```json
{
	"type": "go",
	"cmd": ["/app/proxy", "-c", "/app/config.json"]
}
```

The `config.json` file has these elements:

```json
{
	"log": {
		"format": "json",
		"level": "debug"
	},
	"http": {
		"port": 3000
	},
	"https": {
		"port": 3001,
		"certBucket": "my-tls-certs",
		"host": "example.com"
	},
	"providerRegistries": [
		{
			"provider": "gcp:$projectName:us-central1:us-central1-b",
			"registry": "us-central1-pkg.dev/$projectName/$registryName",
			"serviceAccount": "yeoman@$projectName.iam.gserviceaccount.com"
		}
	],
	"subnets": [
		"127.0.0.0/8",
		"10.128.0.0/20"
	]
}
```
