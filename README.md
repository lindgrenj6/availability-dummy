## So you want to run Sources API locally for testing or something - but do not want to run cost/rhsm/cloudigrade in order for availability checks to work
![Captain America Sitting on a chair lecturing us](https://cdn.vox-cdn.com/thumbor/g4cBNuBxtOTGOed6jQks0av8RGU=/0x0:970x545/1200x800/filters:focal(373x129:527x283)/cdn.vox-cdn.com/uploads/chorus_image/image/59766741/so_meme.0.jpg)

This repository contains a program that basically simulates all of their endpoints - and from there posts random `(available|unavailable)` messages back on the `platform.sources.status` topic.

Best used from minikube.

### To setup in your local bonfire config
Add this snippet to your local configuration:
```yaml
- name: sources-availability
  host: github
  repo: jlindgren/availability-dummy
  path: deploy/clowdapp.yaml
```
from there there should be a `sources-availability-dummy` deployment created by the clowdapp.

Now this isn't very useful if sources-api isn't pointed at it, so I add a few flags to my bonfire command when I want this set up.

I generally just add a few `-p` flags to set my koku/cloudigrade api hosts like so:
```
bonfire process sources \
... other flags \
-p sources-api/KOKU_SOURCES_API_HOST=sources-availability-dummy \
-p sources-api/CLOUD_METER_API_HOST=sources-availability-dummy \
... more flags | sed etc

```
After this sources-api should be configured to hit the dummy endpoint. And you are free to create sources and have them randomly marked available/unavailable when the checkers run!
