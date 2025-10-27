# Little Fighter 2 Remastered Multiplayer Server in Go

## Overview

Reimplementation of the Little Fighter 2 Remastered server, 100% vibe coded, don't expect much from the code. Works just enough to start a game between players (or between 1 player and mirror bot). 

## Features

*   **Lobby System:** Players can join and leave rooms.
*   **Multiplayer Sessions:** The server relays game data between players in a room.
*   **Mirror Bot:** A "mirror bot" can be enabled in rooms to echo player actions, useful for testing and development.

## Getting Started

### Prerequisites

*   Go (version 1.18 or later)

## Configuration

The server's room configuration is managed through the `rooms.json` file. If this file is not present when the server starts, a default configuration will be generated.

### Enabling the Mirror Bot

To enable the mirror bot for a specific room, you can edit the `rooms.json` file. For each room you want to enable the bot in, set the `isMirror` property to `true`.

**Example `rooms.json` entry:**

```json
{
  "id": 1,
  "name": "Room 1",
  "maxPlayers": 8,
  "defaultLatency": 3,
  "isMirror": true
}
```

If you modify `rooms.json` while the server is running, you will need to restart the server for the changes to take effect.
