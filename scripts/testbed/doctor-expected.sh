# shellcheck shell=bash
# The warnings `mp doctor` may give on the testbed, and why. Everything else
# fails a run: full-run.sh's doctor stage and checks.sh (testbed.sh checks,
# the matrix, testbed.sh upgrade) source this file and count any warning it
# does not list as a failure. Not executable on its own; each line is
#
#   expect <where> <check> <detail> <why>
#
#   where   an extended regex on "<vm> <kernel release> <context>": context is
#           deploy (a fresh deploy of this build, as the matrix makes) or
#           upgrade (the package upgrade from the previous release)
#   check   the name of the doctor's check, exactly as it prints it
#   detail  an extended regex on the check's detail
#   why     one line for the logs: what the host lacks and where that is told
#
# A rule says what is known, not what is fine: when the host gets what it
# lacked, checks.sh says the rule can go.

# Oracle Linux 9 and 10 boot the Unbreakable Enterprise Kernel, which has
# Landlock built in but not in its lsm= list (landlock: operation not
# supported): the agent runs restic without its Landlock sandbox, and a
# repository directory that holds other files is not refused, or only after
# restic init wrote into it. What else that leaves open (a backup that
# follows an account's symlinks, unless the agent reads the trees in its
# view without symlinks) the doctor's detail says.
expect 'el[0-9]+uek' 'restic sandbox' '^no Landlock' "UEK boots without landlock in lsm=: restic runs without its Landlock sandbox (grubby --update-kernel=ALL --args=lsm=<the list>,landlock, or boot the Red Hat compatible kernel)"
