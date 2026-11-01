# Off-host scheduling is provisioned explicitly; ordinary checks never contact hosts.
.PHONY: backup-test
backup-test:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_registration_backup.py'

check: backup-test
