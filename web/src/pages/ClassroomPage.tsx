import { useEffect, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { Link, useNavigate, useParams } from "react-router";
import {
  acceptClassroomInvitation,
  createOrg,
  createClassroom,
  createClassroomAssignment,
  deleteClassroom,
  deleteClassroomAssignment,
  fetchClassroomDashboard,
  fetchClassroomInvitation,
  fetchClassroomAcceptedAssignments,
  fetchClassroomGrades,
  fetchRepos,
  exportClassroomTransition,
  importClassroomTransition,
  replaceClassroomRoster,
  updateClassroom,
  updateClassroomAssignment,
  type Classroom,
  type ClassroomAssignment,
  type ClassroomAutogradingTest,
  type ClassroomAcceptedAssignment,
  type ClassroomGrade,
} from "../api.js";
import type { BleephubRepo } from "../types.js";
import { Blankslate, Box, Button, DialogActions, ErrorBanner, FormLabel, Modal, StateLabel } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { GearIcon, OrganizationIcon, PeopleIcon, PlusIcon, RepoIcon } from "../components/octicons.js";

export function ClassroomPage() {
  const { classroomId, inviteCode } = useParams<{ classroomId?: string; inviteCode?: string }>();
  if (inviteCode) return <AcceptAssignment code={inviteCode} />;
  return <ClassroomManagement classroomID={classroomId ? Number(classroomId) : null} />;
}

function ClassroomHero({ onCreate }: { onCreate?: () => void }) {
  return (
    <section
      className="mb-6 flex flex-wrap items-end justify-between gap-5"
      style={{
        padding: "1.5rem",
        color: "#fff",
        borderRadius: "0.85rem",
        border: "1px solid color-mix(in srgb, var(--color-accent) 55%, var(--color-border))",
        background: "linear-gradient(125deg, #6f2cff 0%, #0969da 43%, #00a6c8 72%, #18a957 100%)",
        boxShadow: "0 12px 30px color-mix(in srgb, var(--color-accent) 24%, transparent)",
      }}
    >
      <div style={{ maxWidth: "44rem" }}>
        <div className="mb-2 inline-flex items-center gap-2" style={{ fontSize: ".78rem", fontWeight: 700, letterSpacing: ".04em", textTransform: "uppercase" }}>
          <PeopleIcon size={16} /> Bleephub Education
        </div>
        <h1 style={{ fontSize: "2rem", lineHeight: 1.12, fontWeight: 750 }}>GitHub Classroom, kept alive.</h1>
        <p className="mt-2" style={{ color: "rgba(255,255,255,.9)", maxWidth: "40rem" }}>
          Keep rosters, starter repositories, automatic assignment repositories, feedback pull requests, and GitHub Actions autograding in one familiar workflow.
        </p>
      </div>
      {onCreate && <Button variant="secondary" onClick={onCreate} style={{ background: "#fff", color: "#24292f", borderColor: "rgba(255,255,255,.7)" }}>
        <PlusIcon size={15} /> New classroom
      </Button>}
    </section>
  );
}

function ClassroomManagement({ classroomID }: { classroomID: number | null }) {
  const query = useQuery({ queryKey: ["classrooms"], queryFn: fetchClassroomDashboard });
  const [showCreate, setShowCreate] = useState(false);
  if (query.isLoading) return <Spinner />;
  if (query.isError) return <InlineError title="Failed to load classrooms" detail={String(query.error)} />;
  const classroom = classroomID ? query.data?.classrooms.find((item) => item.id === classroomID) : undefined;
  if (classroomID && !classroom) return <Blankslate title="Classroom not found">You may not administer this classroom.</Blankslate>;
  return (
    <div>
      {!classroom && <><ClassroomHero onCreate={() => setShowCreate(true)} /><TransitionPanel /></>}
      {classroom ? <ClassroomDetail classroom={classroom} /> : <ClassroomGrid classrooms={query.data?.classrooms ?? []} />}
      {showCreate && (
        <CreateClassroomDialog
          organizations={query.data?.organizations ?? []}
          canCreateOrganization={query.data?.can_create_organization ?? false}
          onClose={() => setShowCreate(false)}
        />
      )}
    </div>
  );
}

function TransitionPanel() {
  const client = useQueryClient();
  const [error, setError] = useState<unknown>(null);
  const exportMutation = useMutation({ mutationFn: exportClassroomTransition, onSuccess: (blob) => { const url = URL.createObjectURL(blob); const link = document.createElement("a"); link.href = url; link.download = "bleephub-classroom-transition.json"; link.click(); URL.revokeObjectURL(url); } });
  const importMutation = useMutation({ mutationFn: importClassroomTransition, onSuccess: () => client.invalidateQueries({ queryKey: ["classrooms"] }) });
  return <Box className="mb-6" style={{ borderColor: "color-mix(in srgb, var(--color-brand-purple) 45%, var(--color-border))" }}><div className="flex flex-wrap items-center justify-between gap-4" style={{ padding: "1rem", background: "linear-gradient(100deg, color-mix(in srgb, var(--color-brand-purple) 12%, var(--color-surface)), color-mix(in srgb, var(--color-brand-cyan) 10%, var(--color-surface)))" }}><div><b>Transition from GitHub Classroom</b><p className="mt-1" style={{ color: "var(--color-fg-muted)", fontSize: ".8rem" }}>Import or export the lossless JSON bundle after migrating the referenced starter and student repositories.</p></div><div className="flex flex-wrap gap-2"><Button onClick={() => exportMutation.mutate()}>Export classrooms</Button><label className="inline-flex"><input type="file" accept="application/json,.json" className="sr-only" onChange={async (event) => { try { const file = event.target.files?.[0]; if (!file) return; importMutation.mutate(JSON.parse(await file.text())); } catch (cause) { setError(cause); } }} /><span className="inline-flex cursor-pointer items-center" style={{ padding: ".34rem .85rem", border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", background: "var(--color-bg-subtle)", fontSize: ".82rem", fontWeight: 600 }}>Import transition bundle</span></label></div></div>{(error || exportMutation.error || importMutation.error) && <div style={{ padding: "0 1rem 1rem" }}><ErrorBanner>{String(error || exportMutation.error || importMutation.error)}</ErrorBanner></div>}</Box>;
}

function ClassroomGrid({ classrooms }: { classrooms: Classroom[] }) {
  if (classrooms.length === 0) {
    return <Blankslate icon={<PeopleIcon size={34} />} title="Create your first classroom"><span>Connect an organization, invite a roster, and publish an assignment using the button above.</span></Blankslate>;
  }
  return (
    <div>
      <div className="mb-3 flex items-center justify-between"><h2 style={{ fontSize: "1.1rem", fontWeight: 650 }}>Your classrooms</h2><span style={{ color: "var(--color-fg-muted)", fontSize: ".82rem" }}>{classrooms.length} total</span></div>
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {classrooms.map((classroom, index) => (
          <Link key={classroom.id} to={`/ui/classrooms/${classroom.id}`} style={{ color: "inherit", textDecoration: "none" }}>
            <Box style={{ height: "100%", boxShadow: "var(--shadow-sm)" }}>
              <div style={{ height: 7, background: ["#8250df", "#0969da", "#00a6c8", "#cf4a9c", "#bf8700"][index % 5] }} />
              <div style={{ padding: "1rem" }}>
                <div className="mb-3 flex items-center justify-between gap-3"><OrganizationIcon size={20} /><StateLabel state={classroom.archived ? "draft" : "open"}>{classroom.archived ? "Archived" : "Active"}</StateLabel></div>
                <h3 style={{ fontSize: "1.05rem", fontWeight: 650 }}>{classroom.name}</h3>
                <div className="mt-1" style={{ color: "var(--color-fg-muted)", fontSize: ".82rem" }}>{classroom.organization.login}</div>
                <div className="mt-4 flex gap-5" style={{ fontSize: ".8rem", color: "var(--color-fg-muted)" }}><span><b style={{ color: "var(--color-fg)" }}>{classroom.assignments.length}</b> assignments</span><span><b style={{ color: "var(--color-fg)" }}>{classroom.roster.length}</b> students</span></div>
              </div>
            </Box>
          </Link>
        ))}
      </div>
    </div>
  );
}

function ClassroomDetail({ classroom }: { classroom: Classroom }) {
  const client = useQueryClient();
  const [showAssignment, setShowAssignment] = useState(false);
  const [showRoster, setShowRoster] = useState(false);
  const [showSettings, setShowSettings] = useState(false);
  const [editingAssignment, setEditingAssignment] = useState<ClassroomAssignment | null>(null);
  const [reportingAssignment, setReportingAssignment] = useState<ClassroomAssignment | null>(null);
  const archive = useMutation({ mutationFn: () => updateClassroom(classroom.id, { archived: !classroom.archived }), onSuccess: () => client.invalidateQueries({ queryKey: ["classrooms"] }) });
  return (
    <div>
      <div className="mb-5"><Link to="/ui/classrooms" style={{ color: "var(--color-accent)", fontSize: ".82rem" }}>← Classrooms</Link></div>
      <MutationError of={archive} />
      <section className="mb-5 flex flex-wrap items-start justify-between gap-4" style={{ paddingBottom: "1rem", borderBottom: "1px solid var(--color-border)" }}>
        <div><div className="flex items-center gap-2"><OrganizationIcon size={24} /><h1 style={{ fontSize: "1.55rem", fontWeight: 700 }}>{classroom.name}</h1><StateLabel state={classroom.archived ? "draft" : "open"}>{classroom.archived ? "Archived" : "Active"}</StateLabel></div><p className="mt-1" style={{ color: "var(--color-fg-muted)", fontSize: ".86rem" }}>Owned by {classroom.organization.login}</p></div>
        <div className="flex gap-2"><Button onClick={() => setShowRoster(true)}><PeopleIcon size={15} /> Roster</Button><Button variant="primary" disabled={classroom.archived} onClick={() => setShowAssignment(true)}><PlusIcon size={15} /> New assignment</Button><Button variant="ghost" onClick={() => archive.mutate()}>{classroom.archived ? "Restore" : "Archive"}</Button><Button aria-label="Classroom settings" onClick={() => setShowSettings(true)}><GearIcon size={15} /> Settings</Button></div>
      </section>
      <div className="mb-5 grid gap-3 sm:grid-cols-3">
        {[{ label: "Students", value: classroom.roster.length, color: "#0969da" }, { label: "Accepted repositories", value: classroom.assignments.reduce((n, a) => n + a.accepted, 0), color: "#8250df" }, { label: "Passing", value: classroom.assignments.reduce((n, a) => n + a.passing, 0), color: "#18a957" }].map((stat) => <Box key={stat.label}><div style={{ padding: "1rem", borderLeft: `5px solid ${stat.color}` }}><div style={{ fontSize: "1.5rem", fontWeight: 750 }}>{stat.value}</div><div style={{ color: "var(--color-fg-muted)", fontSize: ".8rem" }}>{stat.label}</div></div></Box>)}
      </div>
      {classroom.assignments.length === 0 ? <Blankslate icon={<RepoIcon size={34} />} title="No assignments yet">Create an individual or group assignment from a real starter repository.</Blankslate> : <div className="grid gap-3">{classroom.assignments.map((assignment) => <Box key={assignment.id}><div className="flex flex-wrap items-center justify-between gap-4" style={{ padding: "1rem" }}><div><div className="flex items-center gap-2"><RepoIcon size={17} /><b>{assignment.title}</b><span style={{ color: "var(--color-fg-muted)", fontSize: ".76rem" }}>{assignment.type}</span></div><div className="mt-2 flex flex-wrap gap-4" style={{ color: "var(--color-fg-muted)", fontSize: ".8rem" }}><span>{assignment.accepted} accepted</span><span>{assignment.submitted} submitted</span><span style={{ color: "var(--gh-open-solid)" }}>{assignment.passing} passing</span></div></div><div className="text-right"><code style={{ display: "block", color: "var(--color-accent)", fontSize: ".75rem" }}>{assignment.invite_link}</code><span style={{ fontSize: ".72rem", color: "var(--color-fg-muted)" }}>{assignment.autograding_tests?.reduce((n, test) => n + test.points, 0) ?? 0} autograding points</span><div className="mt-2 flex justify-end gap-2"><Button size="sm" onClick={() => setReportingAssignment(assignment)}>View submissions</Button><Button size="sm" disabled={classroom.archived} onClick={() => setEditingAssignment(assignment)}>Edit assignment</Button></div></div></div></Box>)}</div>}
      {showRoster && <RosterDialog classroom={classroom} onClose={() => setShowRoster(false)} />}
      {showAssignment && <AssignmentDialog classroom={classroom} onClose={() => setShowAssignment(false)} />}
      {editingAssignment && <AssignmentDialog classroom={classroom} assignment={editingAssignment} onClose={() => setEditingAssignment(null)} />}
      {reportingAssignment && <AssignmentReportingDialog assignment={reportingAssignment} onClose={() => setReportingAssignment(null)} />}
      {showSettings && <ClassroomSettingsDialog classroom={classroom} onClose={() => setShowSettings(false)} />}
    </div>
  );
}

function CreateClassroomDialog({
  organizations,
  canCreateOrganization,
  onClose,
}: {
  organizations: Array<{ login: string; name: string }>;
  canCreateOrganization: boolean;
  onClose: () => void;
}) {
  const client = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [organization, setOrganization] = useState(organizations[0]?.login ?? "");

  useEffect(() => {
    if (!organization && organizations[0]) setOrganization(organizations[0].login);
  }, [organization, organizations]);

  const mutation = useMutation({
    mutationFn: () => createClassroom({ name: name.trim(), organization }),
    onSuccess: (item) => {
      void client.invalidateQueries({ queryKey: ["classrooms"] });
      onClose();
      navigate(`/ui/classrooms/${item.id}`);
    },
  });

  return (
    <Modal title="Create a classroom" onClose={onClose}>
      {organizations.length === 0 ? (
        <CreateClassroomOrganization
          canCreate={canCreateOrganization}
          onCreated={async (login) => {
            await client.invalidateQueries({ queryKey: ["classrooms"] });
            setOrganization(login);
          }}
        />
      ) : (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            mutation.mutate();
          }}
        >
          <FormLabel id="classroom-name">Classroom name</FormLabel>
          <input
            id="classroom-name"
            type="text"
            autoFocus
            className="mb-4 w-full"
            value={name}
            onChange={(event) => setName(event.target.value)}
            placeholder="Introduction to Computer Science"
            required
          />
          <FormLabel id="classroom-org">Organization</FormLabel>
          <select
            id="classroom-org"
            className="w-full"
            value={organization}
            onChange={(event) => setOrganization(event.target.value)}
            required
          >
            {organizations.map((org) => (
              <option key={org.login} value={org.login}>
                {org.name || org.login} ({org.login})
              </option>
            ))}
          </select>
          {mutation.error && <ErrorBanner>{String(mutation.error)}</ErrorBanner>}
          <DialogActions>
            <Button type="button" onClick={onClose}>Cancel</Button>
            <Button
              type="submit"
              variant="primary"
              disabled={!name.trim() || !organization || mutation.isPending}
            >
              {mutation.isPending ? "Creating…" : "Create classroom"}
            </Button>
          </DialogActions>
        </form>
      )}
    </Modal>
  );
}

function CreateClassroomOrganization({
  canCreate,
  onCreated,
}: {
  canCreate: boolean;
  onCreated: (login: string) => Promise<void>;
}) {
  const [login, setLogin] = useState("");
  const [name, setName] = useState("");
  const mutation = useMutation({
    mutationFn: () => createOrg({ login: login.trim(), name: name.trim() || undefined }),
    onSuccess: (org) => onCreated(org.login),
  });

  if (!canCreate) {
    return (
      <Blankslate title="An organization is required">
        <p>You need to administer an organization before you can create a classroom.</p>
        <p className="mt-2">Ask a site administrator to create one and make you an owner.</p>
      </Blankslate>
    );
  }

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        mutation.mutate();
      }}
    >
      <p className="mb-4" style={{ color: "var(--color-fg-muted)", fontSize: ".84rem" }}>
        Every classroom belongs to an organization. Create the organization first; it will be
        selected automatically.
      </p>
      <FormLabel id="classroom-org-login">Organization login</FormLabel>
      <input
        id="classroom-org-login"
        type="text"
        autoFocus
        className="mb-4 w-full"
        value={login}
        onChange={(event) => setLogin(event.target.value)}
        placeholder="computer-science"
        required
      />
      <FormLabel id="classroom-org-name">Organization name</FormLabel>
      <input
        id="classroom-org-name"
        type="text"
        className="w-full"
        value={name}
        onChange={(event) => setName(event.target.value)}
        placeholder="Computer Science"
      />
      {mutation.error && <ErrorBanner>{String(mutation.error)}</ErrorBanner>}
      <DialogActions>
        <Link to="/ui/admin/orgs" style={{ color: "var(--color-accent)", marginRight: "auto" }}>
          Organization settings
        </Link>
        <Button type="submit" variant="primary" disabled={!login.trim() || mutation.isPending}>
          {mutation.isPending ? "Creating…" : "Create organization"}
        </Button>
      </DialogActions>
    </form>
  );
}

function RosterDialog({ classroom, onClose }: { classroom: Classroom; onClose: () => void }) {
  const client = useQueryClient(); const [value, setValue] = useState(classroom.roster.map((entry) => `${entry.login},${entry.roster_identifier}`).join("\n"));
  const mutation = useMutation({ mutationFn: () => replaceClassroomRoster(classroom.id, value.split("\n").filter(Boolean).map((line) => { const [login, ...identifier] = line.split(","); return { login: login.trim(), roster_identifier: identifier.join(",").trim() }; })), onSuccess: () => { client.invalidateQueries({ queryKey: ["classrooms"] }); onClose(); } });
  return <Modal title="Manage roster" onClose={onClose}><p className="mb-3" style={{ color: "var(--color-fg-muted)", fontSize: ".82rem" }}>One student per line: <code>github-login,roster-identifier</code>.</p><textarea className="w-full" rows={10} value={value} onChange={(e) => setValue(e.target.value)} />{mutation.error && <ErrorBanner>{String(mutation.error)}</ErrorBanner>}<DialogActions><Button onClick={onClose}>Cancel</Button><Button variant="primary" onClick={() => mutation.mutate()}>Save roster</Button></DialogActions></Modal>;
}

function ClassroomSettingsDialog({ classroom, onClose }: { classroom: Classroom; onClose: () => void }) {
  const client = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState(classroom.name);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const save = useMutation({
    mutationFn: () => updateClassroom(classroom.id, { name: name.trim() }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["classrooms"] });
      onClose();
    },
  });
  const remove = useMutation({
    mutationFn: () => deleteClassroom(classroom.id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["classrooms"] });
      navigate("/ui/classrooms");
    },
  });

  return (
    <Modal title="Classroom settings" onClose={onClose}>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          save.mutate();
        }}
      >
        <FormLabel id="classroom-settings-name">Classroom name</FormLabel>
        <input
          id="classroom-settings-name"
          className="w-full"
          value={name}
          onChange={(event) => setName(event.target.value)}
          required
        />
        {(save.error || remove.error) && <ErrorBanner>{String(save.error || remove.error)}</ErrorBanner>}
        <DialogActions>
          {confirmDelete ? (
            <>
              <span role="alert" style={{ color: "var(--color-danger-fg)", marginRight: "auto" }}>
                Delete this classroom and its assignments?
              </span>
              <Button type="button" onClick={() => setConfirmDelete(false)}>Keep classroom</Button>
              <Button type="button" variant="danger" disabled={remove.isPending} onClick={() => remove.mutate()}>
                {remove.isPending ? "Deleting…" : "Delete permanently"}
              </Button>
            </>
          ) : (
            <>
              <Button type="button" variant="danger" onClick={() => setConfirmDelete(true)}>Delete classroom</Button>
              <span style={{ marginRight: "auto" }} />
              <Button type="button" onClick={onClose}>Cancel</Button>
              <Button type="submit" variant="primary" disabled={!name.trim() || save.isPending}>
                {save.isPending ? "Saving…" : "Save changes"}
              </Button>
            </>
          )}
        </DialogActions>
      </form>
    </Modal>
  );
}

function AssignmentDialog({
  classroom,
  assignment,
  onClose,
}: {
  classroom: Classroom;
  assignment?: ClassroomAssignment;
  onClose: () => void;
}) {
  const client = useQueryClient();
  const editing = Boolean(assignment);
  const [title, setTitle] = useState(assignment?.title ?? "");
  const [starter, setStarter] = useState("");
  const [type, setType] = useState<"individual" | "group">(assignment?.type ?? "individual");
  const [deadline, setDeadline] = useState(
    assignment?.deadline ? assignment.deadline.slice(0, 16) : "",
  );
  const [invitationsEnabled, setInvitationsEnabled] = useState(
    assignment?.invitations_enabled ?? true,
  );
  const nextTestKey = useRef(0);
  const [tests, setTests] = useState<Array<ClassroomAutogradingTest & { key: number }>>(
    assignment?.autograding_tests?.length
      ? assignment.autograding_tests.map((test) => ({ ...test, key: nextTestKey.current++ }))
      : [{ name: "Tests", command: "go test ./...", points: 10, key: nextTestKey.current++ }],
  );
  const [confirmDelete, setConfirmDelete] = useState(false);
  const repositories = useQuery({
    queryKey: ["classroom-starter-repositories"],
    queryFn: fetchRepos,
    enabled: !editing,
  });

  const mutation = useMutation({
    mutationFn: () => editing
      ? updateClassroomAssignment(assignment!.id, {
          title: title.trim(),
          type,
          invitations_enabled: invitationsEnabled,
          deadline: deadline ? new Date(deadline).toISOString() : undefined,
          autograding_tests: tests.map(({ key: _key, ...test }) => test),
        })
      : createClassroomAssignment(classroom.id, {
          title: title.trim(),
          type,
          starter_code_repository: starter.trim(),
          public_repo: false,
          students_are_repo_admins: false,
          feedback_pull_requests_enabled: true,
          deadline: deadline ? new Date(deadline).toISOString() : undefined,
          autograding_tests: tests.map(({ key: _key, ...test }) => test),
        }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["classrooms"] });
      onClose();
    },
  });
  const remove = useMutation({
    mutationFn: () => deleteClassroomAssignment(assignment!.id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["classrooms"] });
      onClose();
    },
  });

  return (
    <Modal title={editing ? "Edit assignment" : "Create an assignment"} onClose={onClose}>
      <form
        onSubmit={(event: FormEvent) => {
          event.preventDefault();
          mutation.mutate();
        }}
      >
        <FormLabel id="assignment-title">Assignment title</FormLabel>
        <input id="assignment-title" className="mb-3 w-full" value={title} onChange={(event) => setTitle(event.target.value)} required />
        {!editing && (
          <>
            <FormLabel id="assignment-starter">Starter repository</FormLabel>
            <select
              id="assignment-starter"
              className="mb-3 w-full"
              value={starter}
              onChange={(event) => setStarter(event.target.value)}
              required
            >
              <option value="">
                {repositories.isLoading ? "Loading repositories…" : "Select a starter repository"}
              </option>
              {(repositories.data ?? []).map((repository: BleephubRepo) => (
                <option key={repository.id} value={repository.full_name}>
                  {repository.full_name}
                </option>
              ))}
            </select>
            {repositories.isError && (
              <ErrorBanner>Could not load repositories: {String(repositories.error)}</ErrorBanner>
            )}
          </>
        )}
        <div className="mb-3 grid grid-cols-2 gap-3">
          <div>
            <FormLabel id="assignment-type">Assignment type</FormLabel>
            <select id="assignment-type" className="w-full" value={type} onChange={(event) => setType(event.target.value as "individual" | "group")}>
              <option value="individual">Individual</option>
              <option value="group">Group</option>
            </select>
          </div>
          <div>
            <FormLabel id="assignment-deadline">Deadline</FormLabel>
            <input id="assignment-deadline" className="w-full" type="datetime-local" value={deadline} onChange={(event) => setDeadline(event.target.value)} />
          </div>
        </div>
        {editing && (
          <label className="mb-3 flex items-center gap-2" style={{ fontSize: ".84rem" }}>
            <input type="checkbox" checked={invitationsEnabled} onChange={(event) => setInvitationsEnabled(event.target.checked)} />
            Accept new students through the invitation link
          </label>
        )}
        <div className="mb-2 flex items-center justify-between">
          <b style={{ fontSize: ".85rem" }}>Autograding tests</b>
          <Button type="button" size="sm" variant="ghost" onClick={() => setTests([...tests, { name: "", command: "", points: 10, key: nextTestKey.current++ }])}>
            <PlusIcon size={13} /> Add test
          </Button>
        </div>
        {tests.map((test, index) => (
          <Box key={test.key} className="mb-2">
            <div className="grid gap-2" style={{ padding: ".75rem" }}>
              <input aria-label={`Test ${index + 1} name`} value={test.name} onChange={(event) => setTests(tests.map((item, i) => i === index ? { ...item, name: event.target.value } : item))} placeholder="Test name" required />
              <input aria-label={`Test ${index + 1} command`} value={test.command} onChange={(event) => setTests(tests.map((item, i) => i === index ? { ...item, command: event.target.value } : item))} placeholder="Command" required />
              <div className="flex gap-2">
                <input aria-label={`Test ${index + 1} points`} className="min-w-0 flex-1" type="number" min={1} value={test.points} onChange={(event) => setTests(tests.map((item, i) => i === index ? { ...item, points: Number(event.target.value) } : item))} required />
                {tests.length > 1 && <Button type="button" variant="ghost" onClick={() => setTests(tests.filter((_, i) => i !== index))}>Remove test</Button>}
              </div>
            </div>
          </Box>
        ))}
        {(mutation.error || remove.error) && <ErrorBanner>{String(mutation.error || remove.error)}</ErrorBanner>}
        <DialogActions>
          {editing && (confirmDelete ? (
            <>
              <span role="alert" style={{ color: "var(--color-danger-fg)", marginRight: "auto" }}>Delete this assignment?</span>
              <Button type="button" onClick={() => setConfirmDelete(false)}>Keep assignment</Button>
              <Button type="button" variant="danger" disabled={remove.isPending} onClick={() => remove.mutate()}>Delete permanently</Button>
            </>
          ) : (
            <Button type="button" variant="danger" onClick={() => setConfirmDelete(true)}>Delete assignment</Button>
          ))}
          {!confirmDelete && (
            <>
              <span style={{ marginRight: "auto" }} />
              <Button type="button" onClick={onClose}>Cancel</Button>
              <Button type="submit" variant="primary" disabled={!title.trim() || (!editing && !starter.trim()) || mutation.isPending}>
                {mutation.isPending ? "Saving…" : editing ? "Save assignment" : "Create assignment"}
              </Button>
            </>
          )}
        </DialogActions>
      </form>
    </Modal>
  );
}

function AssignmentReportingDialog({
  assignment,
  onClose,
}: {
  assignment: ClassroomAssignment;
  onClose: () => void;
}) {
  const accepted = useQuery<ClassroomAcceptedAssignment[]>({
    queryKey: ["classroom-assignment", assignment.id, "accepted"],
    queryFn: () => fetchClassroomAcceptedAssignments(assignment.id),
  });
  const grades = useQuery<ClassroomGrade[]>({
    queryKey: ["classroom-assignment", assignment.id, "grades"],
    queryFn: () => fetchClassroomGrades(assignment.id),
  });

  const gradeByLogin = new Map(
    (grades.data ?? []).map((grade) => [grade.github_username, grade]),
  );
  return (
    <Modal title={`${assignment.title} submissions`} onClose={onClose}>
      {accepted.isLoading || grades.isLoading ? (
        <Spinner label="Loading assignment submissions" />
      ) : accepted.isError || grades.isError ? (
        <InlineError
          title="Failed to load assignment reporting"
          detail={String(accepted.error ?? grades.error)}
        />
      ) : (accepted.data ?? []).length === 0 ? (
        <Blankslate title="No accepted assignments">
          Share the invitation link with students to create their repositories.
        </Blankslate>
      ) : (
        <div className="flex flex-col gap-2">
          {(accepted.data ?? []).map((submission) => (
            <Box key={submission.id}>
              <div className="flex flex-wrap items-center justify-between gap-3" style={{ padding: ".85rem 1rem" }}>
                <div>
                  <Link
                    to={`/ui/repos/${submission.repository.full_name}`}
                    style={{ color: "var(--color-accent)", fontWeight: 650, textDecoration: "none" }}
                  >
                    {submission.repository.full_name}
                  </Link>
                  <div className="mt-1" style={{ color: "var(--color-fg-muted)", fontSize: ".78rem" }}>
                    {submission.students.map((student) => student.login).join(", ")}
                    {" · "}{submission.commit_count} commits
                  </div>
                </div>
                <div className="text-right">
                  <StateLabel state={submission.passing ? "open" : submission.submitted ? "closed" : "draft"}>
                    {submission.passing ? "passing" : submission.submitted ? "submitted" : "in progress"}
                  </StateLabel>
                  <div className="mt-1" style={{ fontSize: ".78rem" }}>
                    {submission.grade}
                    {submission.students[0] && gradeByLogin.get(submission.students[0].login)
                      ? ` points · ${gradeByLogin.get(submission.students[0].login)!.roster_identifier}`
                      : ""}
                  </div>
                </div>
              </div>
            </Box>
          ))}
        </div>
      )}
      <DialogActions>
        <Button onClick={onClose}>Close</Button>
      </DialogActions>
    </Modal>
  );
}

function AcceptAssignment({ code }: { code: string }) {
  const navigate = useNavigate(); const query = useQuery({ queryKey: ["classroom-invite", code], queryFn: () => fetchClassroomInvitation(code) }); const [group, setGroup] = useState(""); const [rosterIdentifier, setRosterIdentifier] = useState("");
  const mutation = useMutation({ mutationFn: () => acceptClassroomInvitation(code, query.data?.type === "group" ? group : undefined, rosterIdentifier), onSuccess: (result) => navigate(`/ui/repos/${result.repository.full_name}`) });
  if (query.isLoading) return <Spinner />; if (query.isError) return <InlineError title="Assignment invitation unavailable" detail={String(query.error)} />;
  const assignment = query.data!;
  return <div style={{ maxWidth: 680, margin: "2rem auto" }}><ClassroomHero /><Box><div style={{ padding: "1.4rem" }}><div className="mb-2 flex items-center gap-2"><RepoIcon size={22} /><h1 style={{ fontSize: "1.35rem", fontWeight: 700 }}>{assignment.title}</h1></div><p style={{ color: "var(--color-fg-muted)" }}>Accepting creates your real assignment repository from <b>{assignment.starter_code_repository?.full_name}</b>, grants your access, configures feedback, and installs the autograding workflow.</p>{assignment.roster_identifier_required && <div className="mt-4"><FormLabel id="roster-identifier">Your roster identifier</FormLabel><input id="roster-identifier" className="w-full" value={rosterIdentifier} onChange={(e) => setRosterIdentifier(e.target.value)} placeholder="Student ID or email from your course roster" required /></div>}{assignment.type === "group" && <div className="mt-4"><FormLabel id="group-name">Team name</FormLabel><input id="group-name" className="w-full" value={group} onChange={(e) => setGroup(e.target.value)} required /></div>}{mutation.error && <ErrorBanner>{String(mutation.error)}</ErrorBanner>}<div className="mt-5"><Button variant="primary" onClick={() => mutation.mutate()} disabled={mutation.isPending || (assignment.type === "group" && !group.trim()) || (assignment.roster_identifier_required && !rosterIdentifier.trim())}>Accept this assignment</Button></div></div></Box></div>;
}
