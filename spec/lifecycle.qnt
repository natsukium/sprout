// The instance lifecycle protocol of cmd/sprout for a single instance id:
// incarnations of the instance directory, the daemon.lock claim, the control
// socket, the `up` attempt token, the boot record, and the `delete`
// incarnation marker.
//
// Each action is one call into the Go code that runs entirely under the
// lifecycle lock (publishAttempt, prepareUpBoot, prepareStartBoot, stopOne,
// deleteOne), or one step taken with no lock at all (newDeleteTarget, a
// daemon starting to serve or exiting). The lifecycle lock is therefore not a
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

  // Inode of the instance directory currently at the pathname; 0 is absent.
  var dirIno: int
  var nextIno: int
  // daemon.lock holder per incarnation. A removed incarnation keeps its entry:
  // a held fd still locks the unlinked inode.
  var dlock: int -> str
  // Whether an `up` committed instance.json into each incarnation.
  var recorded: int -> bool
  var pc: Proc -> str
  // Incarnation whose daemon.lock each process holds; 0 is none.
  var claimed: Proc -> int
  // Incarnation each process's `up` attempt token was published into.
  var token: Proc -> int
  // Incarnation each process's `delete` confirmation was asked about.
  var marker: Proc -> int
  // Incarnation each daemon was prepared for: the token's for `up`, the claimed
  // one for `start`.
  var bootedFor: Proc -> int
  var lastStep: { proc: str, act: str, ino: int, confirmed: int }

  action init = all {
    dirIno' = 0,
    nextIno' = 1,
    dlock' = 1.to(MAX_INO).mapBy(_ => NONE),
    recorded' = 1.to(MAX_INO).mapBy(_ => false),
    pc' = PROCS.mapBy(_ => "idle"),
    claimed' = PROCS.mapBy(_ => 0),
    token' = PROCS.mapBy(_ => 0),
    marker' = PROCS.mapBy(_ => 0),
    bootedFor' = PROCS.mapBy(_ => 0),
    lastStep' = { proc: NONE, act: "init", ino: 0, confirmed: 0 },
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

  // publishAttempt: create the directory if absent, then publish a token in it.
  action upPublish(p: Proc): bool = all {
    pc.get(p) == "idle",
    dirIno != 0 or nextIno <= MAX_INO,
    val ino = if (dirIno == 0) nextIno else dirIno
    all {
      dirIno' = ino,
      nextIno' = if (dirIno == 0) nextIno + 1 else nextIno,
      token' = token.set(p, ino),
      pc' = pc.set(p, "up.built"),
      lastStep' = { proc: p, act: if (dirIno == 0) "up.publish.created" else "up.publish.existing", ino: ino, confirmed: 0 },
    },
    dlock' = dlock, claimed' = claimed, marker' = marker, bootedFor' = bootedFor, recorded' = recorded,
  }

  // prepareUpBoot, after the build ran unlocked: verify the token, converge
  // with a daemon already serving the same bundle, claim daemon.lock, verify
  // again, commit the record, retire the token.
  action upBoot(p: Proc): bool = all {
    pc.get(p) == "up.built",
    val stale = token.get(p) != dirIno
    val converged = not(stale) and servers.size() > 0
    val held = not(stale or converged) and not(lockFree(dirIno))
    val fails = stale or converged or held
    all {
      dlock' = if (fails) dlock else dlock.set(dirIno, p),
      claimed' = if (fails) claimed else claimed.set(p, dirIno),
      bootedFor' = if (fails) bootedFor else bootedFor.set(p, token.get(p)),
      recorded' = if (fails) recorded else recorded.set(dirIno, true),
      pc' = pc.set(p, if (fails) "idle" else "booting"),
      lastStep' = {
        proc: p,
        act: if (stale) "up.boot.stale" else if (converged) "up.boot.converged" else if (held) "up.boot.held" else "up.boot.ok",
        ino: dirIno,
        confirmed: 0,
      },
    },
    token' = token.set(p, 0),
    dirIno' = dirIno, nextIno' = nextIno, marker' = marker,
  }

  // prepareStartBoot: claim, then boot the bundle already on record.
  action start(p: Proc): bool = all {
    pc.get(p) == "idle",
    val absent = dirIno == 0
    val running = not(absent) and servers.size() > 0
    val held = not(absent or running) and not(lockFree(dirIno))
    val norecord = not(absent or running or held) and not(recorded.get(dirIno))
    val fails = absent or running or held or norecord
    all {
      dlock' = if (fails) dlock else dlock.set(dirIno, p),
      claimed' = if (fails) claimed else claimed.set(p, dirIno),
      bootedFor' = if (fails) bootedFor else bootedFor.set(p, dirIno),
      pc' = pc.set(p, if (fails) "idle" else "booting"),
      lastStep' = {
        proc: p,
        act:
          if (absent) "start.notfound"
          else if (running) "start.running"
          else if (held) "start.held"
          else if (norecord) "start.norecord"
          else "start.ok",
        ino: dirIno,
        confirmed: 0,
      },
    },
    dirIno' = dirIno, nextIno' = nextIno, token' = token, marker' = marker, recorded' = recorded,
  }

  // The daemon binds its control socket only after claiming, so a booting
  // daemon holds daemon.lock without answering PING.
  action daemonServe(p: Proc): bool = all {
    pc.get(p) == "booting",
    pc' = pc.set(p, "serving"),
    lastStep' = { proc: p, act: "daemon.serve", ino: claimed.get(p), confirmed: 0 },
    dirIno' = dirIno, nextIno' = nextIno, dlock' = dlock, claimed' = claimed, token' = token,
    marker' = marker, bootedFor' = bootedFor, recorded' = recorded,
  }

  // The kernel drops the flock however the daemon ends; it takes no lock to exit.
  action daemonExit(p: Proc): bool = all {
    DAEMON.contains(pc.get(p)),
    dlock' = dlock.set(claimed.get(p), NONE),
    claimed' = claimed.set(p, 0),
    bootedFor' = bootedFor.set(p, 0),
    pc' = pc.set(p, "idle"),
    lastStep' = { proc: p, act: "daemon.exit", ino: claimed.get(p), confirmed: 0 },
    dirIno' = dirIno, nextIno' = nextIno, token' = token, marker' = marker, recorded' = recorded,
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
      lastStep' = {
        proc: p,
        act:
          if (running and exits) "stop.exited"
          else if (running) "stop.lingering"
          else if (busy) "stop.busy"
          else "stop.notrunning",
        ino: dirIno,
        confirmed: 0,
      },
    },
    dirIno' = dirIno, nextIno' = nextIno, token' = token, marker' = marker, recorded' = recorded,
  }

  // newDeleteTarget: the unlocked selection the user then confirms.
  action deleteSelect(p: Proc): bool = all {
    pc.get(p) == "idle",
    marker' = if (dirIno == 0) marker else marker.set(p, dirIno),
    pc' = if (dirIno == 0) pc else pc.set(p, "delete.confirmed"),
    lastStep' = {
      proc: p,
      act: if (dirIno == 0) "delete.select.absent" else "delete.select",
      ino: dirIno,
      confirmed: 0,
    },
    dirIno' = dirIno, nextIno' = nextIno, dlock' = dlock, claimed' = claimed, token' = token, bootedFor' = bootedFor,
    recorded' = recorded,
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
      lastStep' = {
        proc: p,
        act:
          if (changed) "delete.changed"
          else if (running and held) "delete.stopped.held"
          else if (running) "delete.stopped.removed"
          else if (held) "delete.held"
          else "delete.removed",
        ino: dirIno,
        confirmed: marker.get(p),
      },
    },
    marker' = marker.set(p, 0),
    nextIno' = nextIno, token' = token, recorded' = recorded,
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
    }
  }

  // A daemon always holds the lock of the directory at the pathname: no
  // delete removed it from under the daemon, and no daemon claimed a husk.
  val daemonOnLiveIncarnation: bool =
    PROCS.forall(p => DAEMON.contains(pc.get(p)) implies claimed.get(p) == dirIno)

  // A stale `up` whose instance was deleted mid-build never boots the
  // replacement with its build.
  val daemonBootedForItsIncarnation: bool =
    PROCS.forall(p => DAEMON.contains(pc.get(p)) implies bootedFor.get(p) == claimed.get(p))

  val singleDaemon: bool =
    tuples(PROCS, PROCS).forall(((p, q)) =>
      (p != q and DAEMON.contains(pc.get(p)) and DAEMON.contains(pc.get(q))) implies claimed.get(p) != claimed.get(q))

  // A delete only ever removes the incarnation the user confirmed.
  val deleteHonorsConfirmation: bool =
    Set("delete.removed", "delete.stopped.removed").contains(lastStep.act) implies lastStep.ino == lastStep.confirmed

  // A stop that reports success leaves nothing answering control, so
  // `stop && rm` compositions are safe.
  val stopSilencesControl: bool =
    Set("stop.exited", "stop.lingering").contains(lastStep.act) implies servers.size() == 0

  val safety: bool = all {
    daemonOnLiveIncarnation,
    daemonBootedForItsIncarnation,
    singleDaemon,
    deleteHonorsConfirmation,
    stopSilencesControl,
  }

  // Witnesses that the simulator reaches the races the invariants are about.
  val staleUpRejected: bool = lastStep.act == "up.boot.stale"
  val swappedDeleteRejected: bool = lastStep.act == "delete.changed"
  val runningDeleted: bool = lastStep.act == "delete.stopped.removed"
  val lingeringBlocksDelete: bool = lastStep.act == "delete.stopped.held"
  val recreated: bool = nextIno > 2
}
