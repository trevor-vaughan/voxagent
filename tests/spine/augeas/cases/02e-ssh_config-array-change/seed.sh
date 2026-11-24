printf 'Host *\n\tCiphers aes128-ctr\n\tMACs hmac-sha2-512,hmac-sha1,hmac-md5\n\tSendEnv LANG\n' > /etc/ssh/ssh_config
