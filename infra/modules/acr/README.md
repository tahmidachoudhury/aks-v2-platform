# acr

**TL;DR:** Will hold the container registry, with one repository per service. Empty until P3-10 (#36) fills it.

The `persistent` root will call it, so images survive the nightly destroy. The AcrPull grant for the kubelet identity (P3-11, #37) depends on the cluster, so it belongs in `platform`, not here.
