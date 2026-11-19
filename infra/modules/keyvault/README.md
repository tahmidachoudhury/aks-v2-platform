# keyvault

**TL;DR:** Will hold the Key Vault and its seeded secrets. Empty until P3-13 (#39) fills it.

The `persistent` root will call it. Soft delete and purge protection hold a deleted vault name for days, so a vault recreated every morning would not work.
