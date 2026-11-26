# The second Tidy skips a path the first already generated a File for.
tidy { '/srv/o': recurse => true, matches => ['*.a'] }
tidy { '/srv/o/sub': recurse => true }
