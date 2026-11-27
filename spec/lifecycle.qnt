// The instance lifecycle protocol of cmd/sprout for a single instance id:
// incarnations of the instance directory, the daemon.lock claim, the control
// socket, the `up` attempt token, the boot record and the bundle it names,
// the `delete` and `prune` incarnation markers, and the worktree an orphan
// check looks for.
//
// Each action is one call into the Go code that runs entirely under the
// lifecycle lock (publishAttempt, prepareUpBoot, prepareStartBoot, stopOne,
// deleteOne, pruneOne), or one step taken with no lock at all
// (newDeleteTarget, pruneCandidates, a daemon starting to serve or exiting,
// the user removing or restoring the worktree). The lifecycle lock is therefore not a
// variable: it is what licenses each locked call to be a single atomic step.
// Interleavings inside a locked call are only with unlocked steps, and those
// cannot change what the call observes except through daemon.lock and the
// control socket, whose changes before or after the call both appear as
// separate traces.
//
// Every wait for another process is zero: a locked call observes the state as
// it finds it, and a daemon that STOP did not end at once lingers in
// "stopping" until its own exit step.
//
// cmd/sprout/lifecycle_model_test.go replays traces of this module against
// the real functions; every `lastStep.act` names the outcome the Go call must
// reproduce.
module lifecycle {
  type Proc = str

  pure val PROCS: Set[Proc] = Set("p1", "p2", "p3")
  // Bounds the incarnations a trace can create, so the maps stay total.
  pure val MAX_INO: int = 4
  pure val NONE: str = ""
  pure val DAEMON: Set[str] = Set("booting", "serving", "stopping")
  pure val BUNDLES: Set[str] = Set("A", "B")

  // Inode of the instance directory currently at the pathname; 0 is absent.
  var dirIno: int
  var nextIno: int
  // daemon.lock holder per incarnation. A removed incarnation keeps its entry:
  // a held fd still locks the unlinked inode.
  var dlock: int -> str
  // Bundle the instance.json an `up` committed into each incarnation names;
  // NONE before any.
  var recBundle: int -> str
  // Whether the workspace the record names exists.
  var worktree: bool
  var pc: Proc -> str
  // Incarnation whose daemon.lock each process holds; 0 is none.
  var claimed: Proc -> int
  // Incarnation each process's `up` attempt token was published into, and the
  // bundle that attempt built.
  var token: Proc -> int
  var building: Proc -> str
  // Incarnation each process's `delete` confirmation was asked about.
  var marker: Proc -> int
  // Incarnation each daemon was prepared for: the token's for `up`, the claimed
  // one for `start`; and the bundle it boots.
  var bootedFor: Proc -> int
  var runs: Proc -> str
  var lastStep: { proc: str, act: str, ino: int, confirmed: int, orphaned: bool }

  pure def mkStep(p: Proc, act: str, ino: int, confirmed: int): { proc: str, act: str, ino: int, confirmed: int, orphaned: bool } =
    { proc: p, act: act, ino: ino, confirmed: confirmed, orphaned: false }

  action init = all {
    dirIno' = 0,
    nextIno' = 1,
    dlock' = 1.to(MAX_INO).mapBy(_ => NONE),
    recBundle' = 1.to(MAX_INO).mapBy(_ => NONE),
    worktree' = true,
    building' = PROCS.mapBy(_ => NONE),
    runs' = PROCS.mapBy(_ => NONE),
    pc' = PROCS.mapBy(_ => "idle"),
    claimed' = PROCS.mapBy(_ => 0),
    token' = PROCS.mapBy(_ => 0),
    marker' = PROCS.mapBy(_ => 0),
    bootedFor' = PROCS.mapBy(_ => 0),
    lastStep' = mkStep(NONE, "init", 0, 0),
  }

  def lockFree(ino: int): bool = ino != 0 and dlock.get(ino) == NONE

  def servers: Set[Proc] = PROCS.filter(q => pc.get(q) == "serving" and claimed.get(q) == dirIno)

  // What a STOP does to the daemons: the one answering control either exits at
  // once or lingers holding daemon.lock.
  def afterStop(exits: bool): (Proc -> str, int -> str, Proc -> int, Proc -> int) =
    if (exits)
      (
        PROCS.mapBy(q => if (servers.contains(q)) "idle" else pc.get(q)),
        if (servers.size() > 0) dlock.set(dirIno, NONE) else dlock,
        PROCS.mapBy(q => if (servers.contains(q)) 0 else claimed.get(q)),
        PROCS.mapBy(q => if (servers.contains(q)) 0 else bootedFor.get(q)),
      )
    else
      (PROCS.mapBy(q => if (servers.contains(q)) "stopping" else pc.get(q)), dlock, claimed, bootedFor)

  // What pruneCandidates lists: a stopped instance whose record names a
  // workspace that is gone.
  def orphanNow: bool =
    dirIno != 0 and servers.size() == 0 and recBundle.get(dirIno) != NONE and not(worktree)

  // publishAttempt: create the directory if absent, then publish a token in
  // it; the build that follows runs unlocked.
  action upPublish(p: Proc): bool = all {
    pc.get(p) == "idle",
    dirIno != 0 or nextIno <= MAX_INO,
    nondet b = BUNDLES.oneOf()
    val ino = if (dirIno == 0) nextIno else dirIno
    all {
      dirIno' = ino,
      nextIno' = if (dirIno == 0) nextIno + 1 else nextIno,
      token' = token.set(p, ino),
      building' = building.set(p, b),
      pc' = pc.set(p, "up.built"),
      lastStep' = mkStep(p, if (dirIno == 0) "up.publish.created" else "up.publish.existing", ino, 0),
    },
    dlock' = dlock, claimed' = claimed, marker' = marker, bootedFor' = bootedFor, recBundle' = recBundle,
    worktree' = worktree, runs' = runs,
  }

  // prepareUpBoot: verify the token, converge with a daemon already serving
  // the same bundle or stop one serving another, claim daemon.lock, verify
  // again, commit the record, retire the token.
  action upBoot(p: Proc): bool = all {
    pc.get(p) == "up.built",
    nondet exits = Set(true, false).oneOf()
    val b = building.get(p)
    val stale = token.get(p) != dirIno
    val running = not(stale) and servers.size() > 0
    val converged = running and recBundle.get(dirIno) == b
    val reboot = running and not(converged)
    val (pc2, dlock2, claimed2, booted2) = if (reboot) afterStop(exits) else (pc, dlock, claimed, bootedFor)
    val held = not(stale or converged) and dlock2.get(dirIno) != NONE
    val fails = stale or converged or held
    all {
      dlock' = if (fails) dlock2 else dlock2.set(dirIno, p),
      claimed' = if (fails) claimed2 else claimed2.set(p, dirIno),
      bootedFor' = if (fails) booted2 else booted2.set(p, token.get(p)),
      runs' = if (fails) runs else runs.set(p, b),
      recBundle' = if (fails) recBundle else recBundle.set(dirIno, b),
      pc' = pc2.set(p, if (fails) "idle" else "booting"),
      lastStep' = mkStep(
        p,
        if (stale) "up.boot.stale"
        else if (converged) "up.boot.converged"
        else if (reboot and held) "up.reboot.held"
        else if (reboot) "up.reboot.ok"
        else if (held) "up.boot.held"
        else "up.boot.ok",
        dirIno,
        0,
      ),
    },
    token' = token.set(p, 0),
    building' = building.set(p, NONE),
    dirIno' = dirIno, nextIno' = nextIno, marker' = marker, worktree' = worktree,
  }

  // prepareStartBoot: claim, then boot the bundle already on record.
  action start(p: Proc): bool = all {
    pc.get(p) == "idle",
    val absent = dirIno == 0
    val running = not(absent) and servers.size() > 0
    val held = not(absent or running) and not(lockFree(dirIno))
    val norecord = not(absent or running or held) and recBundle.get(dirIno) == NONE
    val fails = absent or running or held or norecord
    all {
      dlock' = if (fails) dlock else dlock.set(dirIno, p),
      claimed' = if (fails) claimed else claimed.set(p, dirIno),
      bootedFor' = if (fails) bootedFor else bootedFor.set(p, dirIno),
      runs' = if (fails) runs else runs.set(p, recBundle.get(dirIno)),
      pc' = pc.set(p, if (fails) "idle" else "booting"),
      lastStep' = mkStep(
        p,
        if (absent) "start.notfound"
        else if (running) "start.running"
        else if (held) "start.held"
        else if (norecord) "start.norecord"
        else "start.ok",
        dirIno,
        0,
      ),
    },
    dirIno' = dirIno, nextIno' = nextIno, token' = token, building' = building, marker' = marker,
    recBundle' = recBundle, worktree' = worktree,
  }

  // The daemon binds its control socket only after claiming, so a booting
  // daemon holds daemon.lock without answering PING.
  action daemonServe(p: Proc): bool = all {
    pc.get(p) == "booting",
    pc' = pc.set(p, "serving"),
    lastStep' = mkStep(p, "daemon.serve", claimed.get(p), 0),
    dirIno' = dirIno, nextIno' = nextIno, dlock' = dlock, claimed' = claimed, token' = token,
    building' = building, marker' = marker, bootedFor' = bootedFor, recBundle' = recBundle,
    worktree' = worktree, runs' = runs,
  }

  // The kernel drops the flock however the daemon ends; it takes no lock to exit.
  action daemonExit(p: Proc): bool = all {
    DAEMON.contains(pc.get(p)),
    dlock' = dlock.set(claimed.get(p), NONE),
    claimed' = claimed.set(p, 0),
    bootedFor' = bootedFor.set(p, 0),
    runs' = runs.set(p, NONE),
    pc' = pc.set(p, "idle"),
    lastStep' = mkStep(p, "daemon.exit", claimed.get(p), 0),
    dirIno' = dirIno, nextIno' = nextIno, token' = token, building' = building, marker' = marker,
    recBundle' = recBundle, worktree' = worktree,
  }

  // stopOne without --hard: a daemon serving control is told to STOP; one
  // holding daemon.lock without serving is a stuck boot or a lingering exit,
  // which the zero boot wait reports instead of waiting out.
  action stop(p: Proc): bool = all {
    pc.get(p) == "idle",
    nondet exits = Set(true, false).oneOf()
    val running = servers.size() > 0
    val busy = not(running) and dirIno != 0 and not(lockFree(dirIno))
    val (pc2, dlock2, claimed2, booted2) = if (running) afterStop(exits) else (pc, dlock, claimed, bootedFor)
    all {
      pc' = pc2,
      dlock' = dlock2,
      claimed' = claimed2,
      bootedFor' = booted2,
      runs' = PROCS.mapBy(q => if (claimed2.get(q) == 0) NONE else runs.get(q)),
      lastStep' = mkStep(
        p,
        if (running and exits) "stop.exited"
        else if (running) "stop.lingering"
        else if (busy) "stop.busy"
        else "stop.notrunning",
        dirIno,
        0,
      ),
    },
    dirIno' = dirIno, nextIno' = nextIno, token' = token, building' = building, marker' = marker,
    recBundle' = recBundle, worktree' = worktree,
  }

  // newDeleteTarget: the unlocked selection the user then confirms.
  action deleteSelect(p: Proc): bool = all {
    pc.get(p) == "idle",
    marker' = if (dirIno == 0) marker else marker.set(p, dirIno),
    pc' = if (dirIno == 0) pc else pc.set(p, "delete.confirmed"),
    lastStep' = mkStep(p, if (dirIno == 0) "delete.select.absent" else "delete.select", dirIno, 0),
    dirIno' = dirIno, nextIno' = nextIno, dlock' = dlock, claimed' = claimed, token' = token,
    building' = building, bootedFor' = bootedFor, recBundle' = recBundle, worktree' = worktree, runs' = runs,
  }

  // deleteOne: verify the confirmation, stop a serving daemon, claim, remove.
  action deleteRun(p: Proc): bool = all {
    pc.get(p) == "delete.confirmed",
    nondet exits = Set(true, false).oneOf()
    val changed = marker.get(p) != dirIno
    val running = not(changed) and servers.size() > 0
    val (pc2, dlock2, claimed2, booted2) = if (running) afterStop(exits) else (pc, dlock, claimed, bootedFor)
    val held = not(changed) and dlock2.get(dirIno) != NONE
    all {
      dirIno' = if (changed or held) dirIno else 0,
      pc' = pc2.set(p, "idle"),
      dlock' = dlock2,
      claimed' = claimed2,
      bootedFor' = booted2,
      runs' = PROCS.mapBy(q => if (claimed2.get(q) == 0) NONE else runs.get(q)),
      lastStep' = mkStep(
        p,
        if (changed) "delete.changed"
        else if (running and held) "delete.stopped.held"
        else if (running) "delete.stopped.removed"
        else if (held) "delete.held"
        else "delete.removed",
        dirIno,
        marker.get(p),
      ),
    },
    marker' = marker.set(p, 0),
    nextIno' = nextIno, token' = token, building' = building, recBundle' = recBundle, worktree' = worktree,
  }

  // The user removing the worktree, or checking it out again, with no lock.
  action worktreeToggle(p: Proc): bool = all {
    pc.get(p) == "idle",
    worktree' = not(worktree),
    lastStep' = mkStep(p, if (worktree) "worktree.remove" else "worktree.restore", dirIno, 0),
    dirIno' = dirIno, nextIno' = nextIno, dlock' = dlock, claimed' = claimed, token' = token,
    building' = building, marker' = marker, bootedFor' = bootedFor, recBundle' = recBundle, pc' = pc, runs' = runs,
  }

  // pruneCandidates: the unlocked orphan listing the user then confirms.
  action pruneSelect(p: Proc): bool = all {
    pc.get(p) == "idle",
    marker' = if (orphanNow) marker.set(p, dirIno) else marker,
    pc' = if (orphanNow) pc.set(p, "prune.confirmed") else pc,
    lastStep' = mkStep(p, if (orphanNow) "prune.select" else "prune.select.none", dirIno, 0),
    dirIno' = dirIno, nextIno' = nextIno, dlock' = dlock, claimed' = claimed, token' = token,
    building' = building, bootedFor' = bootedFor, recBundle' = recBundle, worktree' = worktree, runs' = runs,
  }

  // pruneOne: verify the confirmation, claim without waiting, reclassify
  // under the claim, remove. A skip is not an error.
  action pruneRun(p: Proc): bool = all {
    pc.get(p) == "prune.confirmed",
    val changed = marker.get(p) != dirIno
    val held = not(changed) and not(lockFree(dirIno))
    val adopted = not(changed or held) and (recBundle.get(dirIno) == NONE or worktree)
    val skip = changed or held or adopted
    all {
      dirIno' = if (skip) dirIno else 0,
      lastStep' = mkStep(
        p,
        if (changed) "prune.changed" else if (held) "prune.held" else if (adopted) "prune.adopted" else "prune.removed",
        dirIno,
        marker.get(p),
      ).with("orphaned", not(worktree)),
    },
    marker' = marker.set(p, 0),
    pc' = pc.set(p, "idle"),
    nextIno' = nextIno, dlock' = dlock, claimed' = claimed, token' = token, building' = building,
    bootedFor' = bootedFor, recBundle' = recBundle, worktree' = worktree, runs' = runs,
  }

  action step = {
    nondet p = PROCS.oneOf()
    any {
      upPublish(p),
      upBoot(p),
      start(p),
      daemonServe(p),
      daemonExit(p),
      stop(p),
      deleteSelect(p),
      deleteRun(p),
      worktreeToggle(p),
      pruneSelect(p),
      pruneRun(p),
    }
  }

  // A daemon always holds the lock of the directory at the pathname: no
  // delete or prune removed it from under the daemon, and no daemon claimed a
  // husk.
  val daemonOnLiveIncarnation: bool =
    PROCS.forall(p => DAEMON.contains(pc.get(p)) implies claimed.get(p) == dirIno)

  // A stale `up` whose instance was deleted mid-build never boots the
  // replacement with its build.
  val daemonBootedForItsIncarnation: bool =
    PROCS.forall(p => DAEMON.contains(pc.get(p)) implies bootedFor.get(p) == claimed.get(p))

  // The record names the bundle the instance's daemon runs, so `up` converges
  // or reboots by comparing against what is actually running.
  val recordDescribesDaemon: bool =
    PROCS.forall(p => DAEMON.contains(pc.get(p)) implies runs.get(p) == recBundle.get(claimed.get(p)))

  val singleDaemon: bool =
    tuples(PROCS, PROCS).forall(((p, q)) =>
      (p != q and DAEMON.contains(pc.get(p)) and DAEMON.contains(pc.get(q))) implies claimed.get(p) != claimed.get(q))

  // A delete or prune only ever removes the incarnation the user confirmed.
  val removalHonorsConfirmation: bool =
    Set("delete.removed", "delete.stopped.removed", "prune.removed").contains(lastStep.act)
      implies lastStep.ino == lastStep.confirmed

  // Prune removes only what is an orphan when it removes it, whatever changed
  // since the unlocked listing.
  val pruneRemovesOnlyOrphans: bool =
    lastStep.act == "prune.removed" implies lastStep.orphaned

  // A stop that reports success leaves nothing answering control, so
  // `stop && rm` compositions are safe.
  val stopSilencesControl: bool =
    Set("stop.exited", "stop.lingering").contains(lastStep.act) implies servers.size() == 0

  val safety: bool = all {
    daemonOnLiveIncarnation,
    daemonBootedForItsIncarnation,
    recordDescribesDaemon,
    singleDaemon,
    removalHonorsConfirmation,
    pruneRemovesOnlyOrphans,
    stopSilencesControl,
  }

  // Witnesses that the simulator reaches the races the invariants are about.
  val staleUpRejected: bool = lastStep.act == "up.boot.stale"
  val swappedDeleteRejected: bool = lastStep.act == "delete.changed"
  val runningDeleted: bool = lastStep.act == "delete.stopped.removed"
  val lingeringBlocksDelete: bool = lastStep.act == "delete.stopped.held"
  val rebooted: bool = lastStep.act == "up.reboot.ok"
  val pruned: bool = lastStep.act == "prune.removed"
  val adoptedSkipped: bool = lastStep.act == "prune.adopted"
  val recreated: bool = nextIno > 2
}
