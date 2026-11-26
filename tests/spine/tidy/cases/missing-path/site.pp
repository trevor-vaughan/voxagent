# A path that does not exist generates nothing.
tidy { '/srv/missing': recurse => true }
