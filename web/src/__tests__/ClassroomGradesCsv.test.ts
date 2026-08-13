import { describe, it, expect } from "vitest";
import { gradesToCsv } from "../pages/ClassroomPage.js";
import type { ClassroomGrade } from "../api.js";

const grade: ClassroomGrade = {
  assignment_name: "Homework 1",
  assignment_url: "http://x/a/hw1",
  starter_code_url: "http://x/edu/starter",
  github_username: "octostudent",
  roster_identifier: "student-01",
  student_repository_name: "hw1-octostudent",
  student_repository_url: "http://x/edu/hw1-octostudent",
  submission_timestamp: "2026-02-01T00:00:00Z",
  points_awarded: 8,
  points_available: 10,
  group_name: "Team, A",
};

describe("gradesToCsv (GitHub Classroom grade export)", () => {
  it("emits the classroom.github.com column order with a header", () => {
    const csv = gradesToCsv([grade]);
    const [header] = csv.split("\n");
    expect(header).toBe(
      "assignment_name,assignment_url,starter_code_url,github_username,roster_identifier," +
        "student_repository_name,student_repository_url,submission_timestamp,points_awarded,points_available,group_name",
    );
  });

  it("quotes fields and escapes embedded quotes/commas; points render numerically", () => {
    const row = gradesToCsv([grade]).split("\n")[1]!;
    expect(row).toContain('"octostudent"');
    expect(row).toContain('"student-01"');
    expect(row).toContain('"8"'); // points_awarded
    expect(row).toContain('"Team, A"'); // comma stays inside the quoted field
  });

  it("handles an empty grade set (header only)", () => {
    expect(gradesToCsv([])).toBe(
      "assignment_name,assignment_url,starter_code_url,github_username,roster_identifier," +
        "student_repository_name,student_repository_url,submission_timestamp,points_awarded,points_available,group_name",
    );
  });
});
