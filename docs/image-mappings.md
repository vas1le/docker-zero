# Image-to-model mappings

Container names never select the simulated service. `image_names` in an external
cookbook explicitly registers which image references use its model. A bare
repository such as `redis` covers its tags and digests; a tagged/digested entry
matches that specific reference only. Official Docker Hub forms (`redis`,
`library/redis`, `docker.io/library/redis`) are equivalent. Tags remain
case-sensitive, registry ports are respected, and `acme/redis` is **not** the
official Redis image.

To deliberately model a custom image as Nginx, add its repository (for example,
`ghcr.io/acme/web`) to the Nginx cookbook's `image_names`. This registration is an
explicit test fixture, not inspection of the real image's contents. Unknown or
ambiguous mappings return HTTP 501 with the unsupported marker. The same matcher
is used for container creation, simulated pull, and image inspection.
