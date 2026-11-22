#!/usr/bin/env bash
# The whole testbed in one go, the way a release is checked: the package
# upgrade from the previous release on every distribution, the matrix (with
# the hardening checks), three panel-to-panel migrations, the four CMSs on
# every VM, the three foreign sources, doctor everywhere. Prints a summary
# and rewrites .dev/panels.txt and .dev/cms.txt (git-ignored) from the logs.
#
#   full-run.sh                      everything
#   full-run.sh upgrade|matrix|migrate|cms|sources|doctor ...   only these stages
#   full-run.sh checks               the hardening checks once more on the deployed VMs
#
# The upgrade stage goes first: it resets every VM and leaves it upgraded,
# and the matrix resets them again for the stages after it. The doctor stage
# fails on a warning that scripts/testbed/doctor-expected.sh does not list.
set -uo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root" || exit 2
L="$root/.dev/testbed/logs"
mkdir -p "$L"
ver=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null | sed 's/^v//')
today=$(date +%F)
vms=$(bash scripts/testbed/testbed.sh names | tr '\n' ' ')
stages=${*:-"upgrade matrix migrate cms sources doctor"}
summary=()
note() { echo "$*"; summary+=("$*"); }
want() { case " $stages " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }
zone="${TB_DNS_LABEL:-tb}.${TB_ZONE:-}"
# shellcheck disable=SC1091
[ -f .dev/testbed.env ] && . .dev/testbed.env && zone="${TB_DNS_LABEL:-tb}.${TB_ZONE:-}"

if want upgrade; then
	echo "== upgrade from the previous release ($vms)"
	bash scripts/testbed/testbed.sh upgrade > "$L/upgrade.log" 2>&1; note "upgrade exit $?"
	grep -E '^[a-z0-9]+ +(ok|FAIL|SKIP)' "$L/upgrade.log" | sed 's/^/  /'
	for vm in $vms; do
		grep -a '^FAIL ' "$L/upgrade-$vm.log" 2>/dev/null | sed "s/^/    $vm: /"
	done
fi

if want matrix; then
	echo "== matrix ($vms)"
	make testbed-matrix > "$L/matrix.log" 2>&1; note "matrix exit $?"
	grep -E '^[a-z0-9]+ .*(ok|FAIL)$' "$L/matrix.log" | sed 's/^/  /'
	# what the checks of the matrix found
	for vm in $vms; do
		grep -a '^FAIL ' "$L/$vm.log" 2>/dev/null | sed "s/^/    $vm: /"
	done
	# Panel admin passwords, printed once by the bootstrap of each VM.
	{
		echo "# MonoPanel testbed: пароли admin панелей (git-ignored). Прогон $ver, $today."
		echo "# Пароли заданы bootstrap'ом при установке; после make testbed-reset панель ставится заново и пароль меняется."
		echo
		for vm in $vms; do
			pw=$(grep -a -o 'admin password: [^ ]*' "$L/$vm.log" 2>/dev/null | tail -n 1 | cut -d' ' -f3)
			[ -n "$pw" ] && printf '%-11s https://%s.%s:8443/   login: admin   password: %s   # матрица %s, %s\n' "$vm" "$vm" "$zone" "$pw" "$ver" "$today"
		done
	} > .dev/panels.txt.new && (umask 077; mv .dev/panels.txt.new .dev/panels.txt; chmod 600 .dev/panels.txt)
fi

if want checks; then
	echo "== checks on every VM"
	for vm in $vms; do
		(bash scripts/testbed/testbed.sh checks "$vm" > "$L/checks-$vm.log" 2>&1; echo "$?" > "$L/.rc-checks-$vm") &
		sleep 2
	done
	wait
	for vm in $vms; do
		note "checks $vm exit $(cat "$L/.rc-checks-$vm") ($(grep -c '^PASS' "$L/checks-$vm.log") pass, $(grep -c '^FAIL' "$L/checks-$vm.log") fail, $(grep -c '^EXPECTED' "$L/checks-$vm.log") expected, $(grep -c '^SKIP' "$L/checks-$vm.log") skipped)"
		grep '^FAIL ' "$L/checks-$vm.log" | sed 's/^/    /'
		rm -f "$L/.rc-checks-$vm"
	done
fi

if want migrate; then
	echo "== migrations"
	for pair in "ubuntu2404 debian13" "debian12 alma10" "ubuntu2204 ol9"; do
		set -- $pair
		bash scripts/testbed/migrate.sh "$1" "$2" > "$L/migrate-$1-$2.log" 2>&1
		note "migrate $1 → $2 exit $? ($(grep -c '^ok ' "$L/migrate-$1-$2.log") ok, $(grep -c '^FAIL' "$L/migrate-$1-$2.log") fail)"
	done
fi

if want cms; then
	echo "== cms on every VM"
	pids=()
	for vm in $vms; do
		(bash scripts/testbed/testbed.sh cms "$vm" > "$L/cms-$vm.log" 2>&1) &
		pids+=($!)
		sleep 3
	done
	i=0
	for vm in $vms; do
		wait "${pids[$i]}"; rc=$?
		# A CMS that failed on a transient download gets one more try on its own.
		if [ $rc -ne 0 ]; then
			failed=$(grep -o '^FAIL  mp cms install [a-z]*' "$L/cms-$vm.log" | awk '{print $5}' | sort -u | tr '\n' ' ')
			if [ -n "$failed" ]; then
				bash scripts/testbed/testbed.sh cms "$vm" $failed > "$L/cms-$vm-retry.log" 2>&1; rc2=$?
				note "cms $vm exit $rc (fail: $failed) → retry exit $rc2"
				grep '^CREDS ' "$L/cms-$vm-retry.log" >> "$L/cms-$vm.log"
				rc=$rc2
			fi
		fi
		note "cms $vm exit $rc (creds $(grep -c '^CREDS ' "$L/cms-$vm.log"), fail $(grep -c '^FAIL' "$L/cms-$vm.log"), notes $(grep -c '^note ' "$L/cms-$vm.log"))"
		i=$((i + 1))
	done
	{
		echo "# MonoPanel testbed: CMS, установленные панелью (mp cms install) для проверки пресетов (git-ignored). Прогон $ver, $today."
		echo "# Пользователь панели cms, базы cms_<cms>, сайты по HTTP (частная сеть). Битрикс — пробная «Старт», «Чистая установка» из Маркетплейса."
		echo
		for vm in $vms; do
			grep -a '^CREDS ' "$L/cms-$vm.log" 2>/dev/null | awk -v vm="$vm" '{printf "%-11s %-9s %-60s admin %s\n", vm, $2, $3, $5}'
		done
	} > .dev/cms.txt.new && (umask 077; mv .dev/cms.txt.new .dev/cms.txt; chmod 600 .dev/cms.txt)
fi

if want sources; then
	echo "== foreign sources"
	(bash scripts/testbed/sources.sh migrate fastpanel rocky9 > "$L/sources-migrate-fastpanel.log" 2>&1; echo "$?" > "$L/.rc-fastpanel") &
	(bash scripts/testbed/sources.sh migrate bitrixvm debian13 > "$L/sources-migrate-bitrixvm.log" 2>&1; echo "$?" > "$L/.rc-bitrixvm") &
	(bash scripts/testbed/sources.sh migrate bitrixvm7 ubuntu2404 > "$L/sources-migrate-bitrixvm7.log" 2>&1; echo "$?" > "$L/.rc-bitrixvm7") &
	wait
	for s in fastpanel bitrixvm bitrixvm7; do
		note "source $s exit $(cat "$L/.rc-$s") ($(grep -c '^  ok ' "$L/sources-migrate-$s.log") ok, $(grep -c '^  FAIL' "$L/sources-migrate-$s.log") fail)"
		rm -f "$L/.rc-$s"
	done
fi

if want doctor; then
	echo "== doctor (the warnings allowed: scripts/testbed/doctor-expected.sh)"
	ex_where=() ex_name=() ex_detail=() ex_why=()
	expect() { ex_where+=("$1"); ex_name+=("$2"); ex_detail+=("$3"); ex_why+=("$4"); }
	# shellcheck source=doctor-expected.sh
	. scripts/testbed/doctor-expected.sh
	# allowed <"vm kernel context"> <the line after "[WARN] ">: why
	# doctor-expected.sh allows that warning; false when it does not
	allowed() {
		local i rest
		for i in "${!ex_name[@]}"; do
			[[ $1 =~ ${ex_where[$i]} ]] || continue
			[[ $2 == "${ex_name[$i]} "* ]] || continue
			rest=${2#"${ex_name[$i]}"}
			rest=${rest#"${rest%%[! ]*}"}
			[[ $rest =~ ${ex_detail[$i]} ]] || continue
			echo "${ex_why[$i]}"
			return 0
		done
		return 1
	}
	for vm in $vms; do
		# the matrix's deploys: context "deploy" for doctor-expected.sh
		ssh -o BatchMode=yes -o ConnectTimeout=10 "mp-$vm" 'echo "kernel $(uname -r)"; MP_LANG=en mp doctor 2>&1' > "$L/doctor-$vm.log"; rc=$?
		kernel=$(sed -n '1s/^kernel //p' "$L/doctor-$vm.log")
		unexpected=0
		lines=()
		while IFS= read -r line; do
			if why=$(allowed "$vm $kernel deploy" "${line#\[WARN\] }"); then
				lines+=("    expected    $line" "                ($why)")
			else
				unexpected=$((unexpected + 1))
				lines+=("    UNEXPECTED  $line")
			fi
		done < <(grep -a '^\[WARN\]' "$L/doctor-$vm.log")
		note "doctor $vm exit $rc: $(grep -cE '^\[(FAIL|fail)\]' "$L/doctor-$vm.log") fail, $(grep -cE '^\[(WARN|warn)\]' "$L/doctor-$vm.log") warn, $unexpected unexpected — $(grep -o '[^ ]* checks.*' "$L/doctor-$vm.log" | tail -n 1)"
		grep -aE '^\[(FAIL|fail)\]' "$L/doctor-$vm.log" | sed 's/^/    /'
		[ "${#lines[@]}" -eq 0 ] || printf '%s\n' "${lines[@]}"
	done
fi

echo; echo "===== SUMMARY $ver $today"; printf '%s\n' "${summary[@]}"
printf '%s\n' "${summary[@]}" | grep -qE 'exit [1-9]|[1-9][0-9]* fail,|[1-9][0-9]* unexpected' && exit 1 || exit 0
