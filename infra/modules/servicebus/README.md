# servicebus

**TL;DR:** Will hold the Service Bus namespace and the queue with dead-lettering. Empty until P3-12 (#38) fills it.

The `platform` root will call it. Messages left in the queue do not survive the nightly destroy.
