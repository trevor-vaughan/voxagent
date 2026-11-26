# A String max_files passes validation and fails generation (§7).
tidy { '/srv/ms': recurse => true, max_files => '5' }
