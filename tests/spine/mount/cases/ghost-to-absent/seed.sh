mkdir -p /mnt/x
mount -t tmpfs tmpfs /mnt/x
printf "# top\ntmpfs\t/mnt/o\ttmpfs\tdefaults\t0\t0\n\n# bot\n" > /etc/fstab
