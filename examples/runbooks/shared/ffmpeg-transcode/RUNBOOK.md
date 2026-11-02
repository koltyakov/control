---
title: Transcode video with FFmpeg
description: Convert a video using a machine suited to the requested encoding job.
requires: >
  A machine with FFmpeg available and enough resources for the video job.
  If GPU acceleration is requested, its GPU and FFmpeg build need to support
  the chosen encoder.
---

# Transcode video with FFmpeg

Choose a suitable machine for the user's requested output. No particular operating system or GPU is required unless the job calls for one.

1. Inspect the input and requested output, then choose an available machine with suitable resources and FFmpeg support.
2. Check the formats and encoders needed for this job. If the user requests GPU acceleration, verify that it is usable on the selected machine.
3. Transfer the input to that worker and submit a tracked encoding task with a fresh ID and declared output.
4. Check the output artifact and report the result. Reconcile the same task ID after uncertain acceptance rather than replaying the command.

Ask before installing dependencies or making changes outside the requested encoding work.
