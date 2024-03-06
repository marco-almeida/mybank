# gobank

Opinionated folder structure for Go projects where scalability and maintainability are the main concerns.

The [Standard Go Project Layout](https://github.com/golang-standards/project-layout/tree/master) was taken into account as well as opinions from the golang community such as [How To Structure Your Golang (API) Projects!?](https://www.youtube.com/watch?v=EqniGcAijDI).

## Folder Structure

...

## Requirements

- Go 1.22.0 or higher
- Make

## Getting Started

1. Clone the repository

```sh
git clone https://github.com/marco-almeida/go-rest-api/tree/main
```

2. Set the environment variables in the `configs/.env` file according to the template in `configs/example.env`.

3. Run the database

```sh
docker compose -f deploy/docker-compose.yml --env-file configs/.env up -d
```

4. Run the application

```sh
make run
```